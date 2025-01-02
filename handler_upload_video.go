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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	http.MaxBytesReader(w, r.Body, 1<<30)
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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't find video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video owner mismatch", err)
		return
	}

	f, fh, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't read video from FormFile", err)
		return
	}
	defer f.Close()

	mtype := fh.Header.Get("Content-Type")
	media, _, err := mime.ParseMediaType(mtype)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse mediatype", err)
		return
	}
	if media != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Wrong media type", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}

	defer os.Remove("tubely-upload.mp4")
	defer tempFile.Close()

	_, err = io.Copy(tempFile, f)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy to temp file", err)
		return
	}

	aspect, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get aspect ratio", err)
		return
	}

	aspectName := "other"

	if aspect == "16:9" {
		aspectName = "landscape"
	} else if aspect == "9:16" {
		aspectName = "portrait"
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset tempfile pointer", err)
		return
	}

	processed, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process for fast start", err)
		return
	}
	defer os.Remove(processed)

	processedFile, err := os.Open(processed)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed file", err)
		return
	}

	defer processedFile.Close()

	rs := make([]byte, 32)
	_, err = rand.Read(rs)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't random", err)
		return
	}

	newFileName := base64.RawURLEncoding.EncodeToString(rs)
	ext := strings.Split(mtype, "/")[1]
	filename := fmt.Sprintf("%s/%s.%s", aspectName, newFileName, ext)

	params := s3.PutObjectInput{Bucket: &cfg.s3Bucket, Key: &filename, Body: tempFile, ContentType: &media}

	_, err = cfg.s3Client.PutObject(r.Context(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Couldn't put object in S3 bucket: %s", cfg.s3Bucket), err)
		return
	}

	newUrl := fmt.Sprintf("%s,%s", cfg.s3Bucket, filename)
	video.VideoURL = &newUrl

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update db with video", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {

	c := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buf := bytes.NewBuffer([]byte{})
	c.Stdout = buf
	err := c.Run()
	if err != nil {
		return "", err
	}

	type Stream struct {
		DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
	}

	type FFProbeOutput struct {
		Streams []Stream `json:"streams"`
	}

	s := &FFProbeOutput{}

	err = json.Unmarshal(buf.Bytes(), s)
	if err != nil {
		return "", err
	}
	if s.Streams[0].DisplayAspectRatio == "16:9" || s.Streams[0].DisplayAspectRatio == "9:16" {
		return s.Streams[0].DisplayAspectRatio, nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	output := fmt.Sprintf("%s.processing", filePath)
	c := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", output)

	var stderr bytes.Buffer
	c.Stderr = &stderr

	err := c.Run()
	if err != nil {
		return "", fmt.Errorf("error process video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(output)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return output, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) < 2 {
		return video, nil
	}
	bucket := parts[0]
	key := parts[1]
	presigned, err := generatePresignedURL(cfg.s3Client, bucket, key, 5*time.Minute)
	if err != nil {
		return video, err
	}
	video.VideoURL = &presigned
	return video, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	presignedUrl, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %v", err)
	}
	return presignedUrl.URL, nil
}

// 	processedFilePath := fmt.Sprintf("%s.processing", inputFilePath)
//
// 	cmd := exec.Command("ffmpeg", "-i", inputFilePath, "-movflags", "faststart", "-codec", "copy", "-f", "mp4", processedFilePath)
// 	var stderr bytes.Buffer
// 	cmd.Stderr = &stderr
//
// 	if err := cmd.Run(); err != nil {
// 		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
// 	}
//
// 	fileInfo, err := os.Stat(processedFilePath)
// 	if err != nil {
// 		return "", fmt.Errorf("could not stat processed file: %v", err)
// 	}
// 	if fileInfo.Size() == 0 {
// 		return "", fmt.Errorf("processed file is empty")
// 	}
//
// 	return processedFilePath, nil
// }
