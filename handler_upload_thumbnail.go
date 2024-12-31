package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
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

	// TODO: implement the upload here
	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse request", err)
		return
	}

	f, fh, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't read thumbnail from FormFile", err)
		return
	}

	vid, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Bad video id", err)
		return
	}

	mtype := fh.Header.Get("Content-Type")
	media, _, err := mime.ParseMediaType(mtype)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse mediatype", err)
		return
	}
	if media != "image/jpg" && media != "image/png" {
		respondWithError(w, http.StatusBadRequest, "Wrong media type", err)
		return
	}
	ext := strings.Split(mtype, "/")[1]
	filename := fmt.Sprintf("%s.%s", videoIDString, ext)
	pth := filepath.Join(cfg.assetsRoot, filename)
	newfile, err := os.Create(pth)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't create file", err)
		return
	}

	_, err = io.Copy(newfile, f)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't copy file", err)
		return
	}

	v, err := cfg.db.GetVideo(vid)

	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't find video", err)
		return
	}

	url := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, filename)

	v.ThumbnailURL = &url

	cfg.db.UpdateVideo(v)

	respondWithJSON(w, http.StatusOK, v)
}
