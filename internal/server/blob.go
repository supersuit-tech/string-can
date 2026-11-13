package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/files"
	"github.com/supersuit-tech/string-can/internal/push"
)

func blobPath(path string) (key, sha string, ok bool) {
	key, rest, found := strings.Cut(path, "/blob/")
	if !found || key == "" || rest == "" || strings.Contains(rest, "/") {
		return "", "", false
	}
	var err error
	key, err = canon.NormalizeKey(key)
	if err != nil {
		return "", "", false
	}
	sha, err = canon.NormalizeSHA(rest)
	if err != nil {
		return "", "", false
	}
	return key, sha, true
}

func rejectOversizedFiles(list []canon.File) error {
	if len(list) == 0 {
		return nil
	}
	max, err := files.MaxBytes()
	if err != nil {
		return err
	}
	for _, f := range list {
		if f.Size > max {
			return errors.New(f.Name + " is over the file size limit")
		}
	}
	return nil
}

func (s *Server) blobs() (*files.Store, error) {
	return files.Open(s.dir.Path)
}

// handleGetBlob serves a blob referenced by a verified message in this thread.
// The path key is the peer, matching GET /<peer>.
func (s *Server) handleGetBlob(w http.ResponseWriter, _ *http.Request, peer, sha string) {
	_, ok, err := s.store.FileInThread(peer, sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	blobs, err := s.blobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	rc, err := blobs.Open(sha)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not available")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// handlePostBlob accepts file bytes from a contact, after the signed message
// that names the hash. The hash is checked before the blob is kept.
func (s *Server) handlePostBlob(w http.ResponseWriter, r *http.Request, sender, sha string) {
	ok, err := s.store.IsContact(sender)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, push.ForbiddenContact)
		return
	}
	f, found, err := s.store.FileFromSender(sender, sha)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "file is not attached to a message")
		return
	}
	max, err := files.MaxBytes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if f.Size > max {
		writeError(w, http.StatusRequestEntityTooLarge, f.Name+" is over the file size limit")
		return
	}
	blobs, err := s.blobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	existed := blobs.Has(sha)
	body := io.Reader(r.Body)
	if f.Size > 0 {
		body = http.MaxBytesReader(w, r.Body, f.Size)
	} else {
		body = http.MaxBytesReader(w, r.Body, 1)
	}
	if err := blobs.SaveVerified(f.Name, sha, f.Size, body); err != nil {
		var mismatch *files.HashMismatch
		if errors.As(err, &mismatch) || isTooLarge(err) {
			if isTooLarge(err) {
				writeError(w, http.StatusRequestEntityTooLarge, f.Name+" is over the file size limit")
				return
			}
			writeError(w, http.StatusBadRequest, mismatch.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"ok": true, "sha256": sha})
}
