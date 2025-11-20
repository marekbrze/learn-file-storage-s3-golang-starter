package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type FFProbeResult struct {
	Streams []Stream `json:"streams"`
}

type Stream struct {
	Index        int    `json:"index"`
	CodecName    string `json:"codec_name"`
	CodecType    string `json:"codec_type"`
	Width        int    `json:"width,omitempty"`  // Only for video streams
	Height       int    `json:"height,omitempty"` // Only for video streams
	AvgFrameRate string `json:"avg_frame_rate,omitempty"`
	Duration     string `json:"duration,omitempty"`    // Duration in seconds as a string
	BitRate      string `json:"bit_rate,omitempty"`    // Bit rate as a string
	SampleRate   string `json:"sample_rate,omitempty"` // Only for audio streams
	Channels     int    `json:"channels,omitempty"`    // Only for audio streams
}

func getAspectRatio(width, height int) (string, error) {
	if width == 16*height/9 {
		return "16:9", nil
	} else if height == 16*width/9 {
		return "9:16", nil
	}
	return "other", nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("cmd.Run() failed with %s", err)
	}
	var result FFProbeResult
	err = json.Unmarshal(stdout.Bytes(), &result)
	if err != nil {
		return "", fmt.Errorf("error when unmarshalling to stream struct: %v", err)
	}
	ratio, err := getAspectRatio(result.Streams[0].Width, result.Streams[0].Height)
	if err != nil {
		return "", fmt.Errorf("error when calculating dimensions: %v", err)
	}
	return ratio, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	videoFromDB, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Something went wrong", err)
		return
	}
	if videoFromDB.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	const maxMemory = 10 << 30
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	mediatype, _, err := mime.ParseMediaType(contentType)
	if mediatype != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	ratio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get aspect ratio", err)
		return
	}

	key := make([]byte, 32)
	_, err = rand.Read(key)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	var prefix string
	if ratio == "16:9" {
		prefix = "landscape"
	} else if ratio == "9:16" {
		prefix = "portrait"
	} else {
		prefix = "other"
	}
	encodedString := base64.RawURLEncoding.EncodeToString(key) + ".mp4"
	keyPath := path.Join(prefix, encodedString)
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	defer os.Remove(processedFilePath)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not open processed file", err)
		return
	}
	defer processedFile.Close()

	awsObjectParams := s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(keyPath),
		Body:        processedFile,
		ContentType: aws.String(mediatype),
	}
	_, err = cfg.s3Client.PutObject(context.Background(), &awsObjectParams)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Something went wrong", err)
		return
	}
	url := fmt.Sprintf("https://%s/%s", cfg.s3CfDistribution, keyPath)
	videoFromDB.VideoURL = &url

	err = cfg.db.UpdateVideo(videoFromDB)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Something went wrong", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoFromDB)
}

func processVideoForFastStart(filePath string) (string, error) {
	processedFilePath := fmt.Sprintf("%s.processing", filePath)

	cmd := exec.Command("ffmpeg", "-i", filePath, "-movflags", "faststart", "-codec", "copy", "-f", "mp4", processedFilePath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(processedFilePath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return processedFilePath, nil
}
