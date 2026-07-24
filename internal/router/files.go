package router

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/FINWAX/tg-control-api-server/internal/upload"
)

// File upload endpoints, served by the gateway directly onto the shared uploads
// volume (no worker relay). Two flows share POST /v1/files:
//
//   - single-shot: send the whole body -> {path, name, size}
//   - resumable:   send header Upload-Length (no body) to start, then PATCH
//     chunks by Upload-Offset, GET to resume, POST .../complete to finalize.
//
// The returned path is an inputFileLocal path the owning worker reads back from
// the same volume.

func (rt *Router) registerFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/files", rt.createOrUpload)
	mux.HandleFunc("PATCH /v1/files/{id}", rt.appendChunk)
	mux.HandleFunc("GET /v1/files/{id}", rt.fileStatus)
	mux.HandleFunc("POST /v1/files/{id}/complete", rt.completeUpload)
	mux.HandleFunc("DELETE /v1/files/{id}", rt.abortUpload)
}

// isFilesPath reports whether a path is a file-upload route (allowed for any
// enabled token, not just master — see auth).
func isFilesPath(p string) bool {
	return p == "/v1/files" || strings.HasPrefix(p, "/v1/files/")
}

// createOrUpload starts a resumable upload when Upload-Length is present,
// otherwise streams a single-shot body straight to disk.
func (rt *Router) createOrUpload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if err := upload.ValidateName(name); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid file name (basename only, <=255 bytes)")
		return
	}
	if lh := r.Header.Get("Upload-Length"); lh != "" {
		length, err := strconv.ParseInt(lh, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid Upload-Length")
			return
		}
		id, err := rt.uploads.Create(name, length)
		if err != nil {
			writeUploadErr(w, err)
			return
		}
		writeOK(w, map[string]any{"upload_id": id, "chunk_size": rt.uploads.MaxChunk()})
		return
	}
	// Single-shot: cap the body so an oversize payload is refused, not spooled.
	r.Body = http.MaxBytesReader(w, r.Body, rt.uploads.MaxSingle())
	path, size, err := rt.uploads.SingleShot(name, r.Body)
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	writeOK(w, map[string]any{"path": path, "name": name, "size": size})
}

func (rt *Router) appendChunk(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, http.StatusBadRequest, "invalid Upload-Offset")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, rt.uploads.MaxChunk())
	newOffset, err := rt.uploads.Append(r.PathValue("id"), offset, r.Body)
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	writeOK(w, map[string]any{"offset": newOffset})
}

func (rt *Router) fileStatus(w http.ResponseWriter, r *http.Request) {
	offset, length, err := rt.uploads.Status(r.PathValue("id"))
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	writeOK(w, map[string]any{"offset": offset, "length": length})
}

func (rt *Router) completeUpload(w http.ResponseWriter, r *http.Request) {
	path, size, err := rt.uploads.Complete(r.PathValue("id"))
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	writeOK(w, map[string]any{"path": path, "size": size})
}

func (rt *Router) abortUpload(w http.ResponseWriter, r *http.Request) {
	if err := rt.uploads.Abort(r.PathValue("id")); err != nil {
		writeUploadErr(w, err)
		return
	}
	writeOK(w, map[string]any{"status": "deleted"})
}

// writeUploadErr maps an upload-store error to an HTTP status.
func writeUploadErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, upload.ErrBadName), errors.Is(err, upload.ErrBadID):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, upload.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, upload.ErrBadOffset), errors.Is(err, upload.ErrIncomplete):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, upload.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
	default:
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "body exceeds allowed size")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
