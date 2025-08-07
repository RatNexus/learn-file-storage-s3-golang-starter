package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20 // 10MB
	r.ParseMultipartForm(maxMemory)

	// "thumbnail" should match the HTML form input name
	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest,
			"Missing 'thumbnail' form field", err)
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

	if !(mediaType == "image/jpeg" || mediaType == "image/png") {
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
			"File exceeds maximum size of 10 MiB", nil)
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
		fmt.Println("Error generating random bytes:", err)
		return
	}

	fileName := base64.RawURLEncoding.EncodeToString(randomBytes) + `.` + extension
	newFilepath := filepath.Join(cfg.assetsRoot, fileName)

	nFile, err := os.Create(newFilepath)
	if err != nil {
		respondWithError(w, http.StatusBadRequest,
			"Failed to create file", err)
		return
	}
	nFile.Write(rawData)
	defer nFile.Close()

	thumbnailStr := fmt.Sprintf(
		"http://localhost:%s/%s", cfg.port, newFilepath)
	videoGot.ThumbnailURL = &thumbnailStr

	fmt.Println(thumbnailStr)
	err = cfg.db.UpdateVideo(videoGot)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError,
			"Failed to update video record", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoGot)
}
