package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type Resolution struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Aspect string `json:"display_aspect_ratio"`
}

func processVideoForFastStart(filePath string) (string, error) {
	newFilePath := filePath + ".processing"
	fmt.Println("OLD: ", filePath)
	fmt.Println("NEW: ", newFilePath)

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newFilePath)
	out := bytes.Buffer{}
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return newFilePath, nil
}

func getVideoAspectRatio(filePath string) (string, error) {

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	out := bytes.Buffer{}
	cmd.Stdout = &out
	cmd.Run()

	var data struct {
		Streams []Resolution `json:"streams"`
	}

	err := json.Unmarshal(out.Bytes(), &data)
	if err != nil {
		return "", err
	}
	aspect := data.Streams[0].Aspect

	fmt.Printf("Aspect: %s\n", aspect)

	if aspect != "16:9" && aspect != "9:16" {
		return "other", nil
	}

	return aspect, nil
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

	fmt.Println("uploading video file for video", videoID, "by user", userID)

	const maxMemory = 1 << 30 // 1GB
	r.ParseMultipartForm(maxMemory)

	// "video" should match the HTML form input name
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest,
			"Missing 'video' form field", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	if mediaType == "" {
		respondWithError(w, http.StatusBadRequest,
			"Content-Type header not provided", nil)
		return
	}

	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest,
			"Content-Type error", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest,
			"Illegal Content-Type", err)
		return
	}

	extension := strings.Split(mediaType, "/")[1]

	rawData, err := io.ReadAll(io.LimitReader(file, maxMemory+1))
	if err != nil {
		respondWithError(w, http.StatusBadRequest,
			"Failed to read file data", err)
		return
	}
	if int64(len(rawData)) > maxMemory {
		respondWithError(w, http.StatusRequestEntityTooLarge,
			"File exceeds maximum size of 1GB", nil)
		return
	}

	videoGot, err := cfg.db.GetVideo(videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondWithError(w, http.StatusNotFound,
				"Video not found", err)
		} else {
			respondWithError(w, http.StatusInternalServerError,
				"Database error while fetching video", err)
		}
		return
	}

	if videoGot.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	tempTempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp file", err)
		return
	}

	tempFileName, err := processVideoForFastStart(tempTempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error fast processing vid", err)
		return
	}

	os.Remove(tempTempFile.Name())
	tempTempFile.Close()

	tempFile, err := os.Open(tempFileName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating fast vid file", err)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	tempFile.Write(rawData)
	tempFile.Seek(0, io.SeekStart)

	/*
		thumb := thumbnail{
			data:      rawData,
			mediaType: mediaType,
		}

		videoThumbnails[videoID] = thumb
	*/
	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating random bytes:", err)
		return
	}

	fileName := base64.RawURLEncoding.EncodeToString(randomBytes) + `.` + extension

	prefix := "other"
	aspect, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error geting aspect ratio", err)
		return
	}

	if aspect == "16:9" {
		prefix = "landscape"
	} else if aspect == "9:16" {
		prefix = "portrait"
	}

	fileName = fmt.Sprintf("%s/%s", prefix, fileName)

	s3VidPath := fmt.Sprintf(
		"https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fileName)
	videoGot.VideoURL = &s3VidPath

	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileName,
		Body:        tempFile,
		ContentType: &mediaType,
	})
	if err != nil {
		fmt.Println("Error on PutObject:", err)
		return
	}

	fmt.Println(s3VidPath)
	err = cfg.db.UpdateVideo(videoGot)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError,
			"Failed to update video record", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoGot)
}

// TODO: Check if not removing the file/ closing / we will help
// after stop of removal try using the ffmpeg command again
// ffmpeg -i "boots.mp4" -c copy -movflags faststart -f mp4 "poots.mp4.temp"
