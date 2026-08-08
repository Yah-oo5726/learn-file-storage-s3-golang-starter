package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"

	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type ffprobeOutput struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	stdoutBuffer := bytes.Buffer{}
	cmd.Stdout = &stdoutBuffer
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var output ffprobeOutput
	err = json.Unmarshal(stdoutBuffer.Bytes(), &output)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal ffprobe output: %w", err)
	}

	if len(output.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}

	width := output.Streams[0].Width
	height := output.Streams[0].Height
	var aspectRatio string
	if height == 0 {
		return "", fmt.Errorf("height is zero, cannot calculate aspect ratio")
	}
	if width == 0 {
		return "", fmt.Errorf("width is zero, cannot calculate aspect ratio")
	}
	landscapeCloseness := math.Abs(float64(width*9 - height*16))
	if landscapeCloseness <= 200 {
		aspectRatio = "16:9"
	}
	portraitCloseness := math.Abs(float64(width*16 - height*9))
	if portraitCloseness <= 200 {
		aspectRatio = "9:16"
	}
	if aspectRatio == "" {
		aspectRatio = "other"
	}

	return aspectRatio, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<30) // 1 GB limit
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
		respondWithError(w, http.StatusUnauthorized, "Invalid JWT", err)
		return
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You do not have permission to upload this video", nil)
		return
	}

	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to read video file", err)
		return
	}
	defer file.Close()

	fileType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to parse Content-Type", err)
		return
	}
	if fileType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid video format", nil)
		return
	}
	tempFile, err := os.CreateTemp("", "video-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temporary file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save video file", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to seek temporary file", err)
		return
	}
	filenameBytes := make([]byte, 16)
	_, err = rand.Read(filenameBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate filename", err)
		return
	}
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get video aspect ratio", err)
		return
	}
	var aspectRatioPrefix string
	if aspectRatio != "16:9" && aspectRatio != "9:16" {
		aspectRatioPrefix = "other"
	}
	if aspectRatio == "16:9" {
		aspectRatioPrefix = "landscape"
	}
	if aspectRatio == "9:16" {
		aspectRatioPrefix = "portrait"
	}
	filename := base64.RawURLEncoding.EncodeToString(filenameBytes)
	filenameWithPrefix := fmt.Sprintf("%s/%s.mp4", aspectRatioPrefix, filename)
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &filenameWithPrefix,
		Body:        tempFile,
		ContentType: &fileType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload video to S3", err)
		return
	}
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, filenameWithPrefix)
	video.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video record", err)
		return
	}
}
