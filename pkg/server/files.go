package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Raw-bytes file routes. Bodies are streamed to and from disk, so memory use
// does not grow with the file size, and errors use HTTP status codes with a
// {"error": "..."} body. The JSON routes (/write_file, /read_file, ...) are kept
// as they are for older clients.

type PutFileResponse struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// FileEntry describes a file in /files/stat and /files/list responses.
type FileEntry struct {
	Path   string `json:"path,omitempty"`
	Type   string `json:"type,omitempty"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode,omitempty"`
	MTime  string `json:"mtime,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Error is set on a /files/list entry that could not be read; the listing goes on.
	Error string `json:"error,omitempty"`
}

func (s *Server) registerFileRoutes(mux *http.ServeMux) {
	// A GET pattern also matches HEAD.
	mux.Handle("GET /files", s.authMiddleware(http.HandlerFunc(s.getFileHandler)))
	mux.Handle("PUT /files", s.authMiddleware(http.HandlerFunc(s.putFileHandler)))
	mux.Handle("GET /files/stat", s.authMiddleware(http.HandlerFunc(s.statFileHandler)))
	mux.Handle("GET /files/list", s.authMiddleware(http.HandlerFunc(s.listFilesHandler)))
}

func writeFileError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// fsErrorStatus maps a filesystem error to an HTTP status.
func fsErrorStatus(err error) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrPermission):
		return http.StatusForbidden
	case errors.Is(err, syscall.ENOTDIR),
		errors.Is(err, syscall.EISDIR),
		errors.Is(err, syscall.ENOTEMPTY):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeFSError(w http.ResponseWriter, err error) {
	writeFileError(w, fsErrorStatus(err), err.Error())
}

func pathParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeFileError(w, http.StatusBadRequest, "path is required")
		return "", false
	}
	return filepath.Clean(p), true
}

func fileType(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "file"
	case mode.IsDir():
		return "dir"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	default:
		return "other"
	}
}

func fileEntry(rel string, info fs.FileInfo) FileEntry {
	return FileEntry{
		Path:  rel,
		Type:  fileType(info.Mode()),
		Size:  info.Size(),
		Mode:  fmt.Sprintf("%04o", info.Mode().Perm()),
		MTime: info.ModTime().UTC().Format(time.RFC3339Nano),
	}
}

func (s *Server) getFileHandler(w http.ResponseWriter, r *http.Request) {
	path, ok := pathParam(w, r)
	if !ok {
		return
	}

	// Stat before opening: open(2) on a FIFO would block until a writer
	// appears, so the 400 for non-regular files has to come first.
	info, err := os.Stat(path)
	if err != nil {
		slog.Debug("Failed to stat file", "path", path, "error", err)
		writeFSError(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		writeFileError(w, http.StatusBadRequest, fmt.Sprintf("%s is a %s, not a regular file", path, fileType(info.Mode())))
		return
	}

	f, err := os.Open(path)
	if err != nil {
		slog.Debug("Failed to open file", "path", path, "error", err)
		writeFSError(w, err)
		return
	}
	defer f.Close()

	slog.Debug("Serving file", "path", path, "bytes", info.Size(), "range", r.Header.Get("Range"))
	w.Header().Set("Content-Type", "application/octet-stream")

	// Pseudo-files (/proc, /sys) report a size of 0 but have content: stream
	// them as they read, without a length, a validator or Range.
	if info.Size() == 0 {
		if r.Method != http.MethodHead {
			if _, err := io.Copy(w, f); err != nil {
				slog.Debug("Failed to stream file", "path", path, "error", err)
			}
		}
		return
	}

	// ServeContent handles Range, If-Range, If-None-Match, If-Modified-Since and
	// HEAD. The ETag must be strong for If-Range to match it.
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano()))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// putReq holds a PUT's parsed request parameters.
type putReq struct {
	path    string
	mode    fs.FileMode
	modeSet bool
	wantSum string
	parents bool
}

// putDest holds the destination a PUT writes to, with its final mode.
type putDest struct {
	path string
	dir  string
	name string
	mode fs.FileMode
}

// parsePutReq validates a PUT's path, mode, checksum and parents parameters.
func parsePutReq(w http.ResponseWriter, r *http.Request) (putReq, bool) {
	path, ok := pathParam(w, r)
	if !ok {
		return putReq{}, false
	}
	query := r.URL.Query()

	req := putReq{path: path, parents: query.Get("parents") == "1"}
	if query.Get("mode") != "" {
		m, err := strconv.ParseUint(query.Get("mode"), 8, 32)
		if err != nil || m > 0o777 {
			writeFileError(w, http.StatusBadRequest, "mode must be octal permission bits, e.g. 0644")
			return putReq{}, false
		}
		req.mode, req.modeSet = fs.FileMode(m), true
	} else {
		req.mode = 0o644
	}

	req.wantSum = strings.ToLower(r.Header.Get("X-Content-SHA256"))
	if req.wantSum != "" {
		if b, err := hex.DecodeString(req.wantSum); err != nil || len(b) != sha256.Size {
			writeFileError(w, http.StatusBadRequest, "X-Content-SHA256 must be 64 hex characters")
			return putReq{}, false
		}
	}
	return req, true
}

// resolvePutDest follows symlinks to the write target, refuses a directory in
// the way, and keeps the replaced file's mode when the request didn't set one.
func resolvePutDest(w http.ResponseWriter, req putReq) (putDest, bool) {
	path, err := writeTarget(req.path)
	if err != nil {
		writeFSError(w, err)
		return putDest{}, false
	}
	dest := putDest{path: path, mode: req.mode}
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			writeFileError(w, http.StatusConflict, fmt.Sprintf("%s is a directory", path))
			return putDest{}, false
		}
		if !req.modeSet {
			dest.mode = info.Mode().Perm()
		}
	}
	dest.dir, dest.name = filepath.Split(path)
	if dest.dir == "" {
		dest.dir = "."
	}
	return dest, true
}

// streamBody copies the request body to tmp while hashing it, then checks the
// optional checksum. A body read error is the client's; a write error is the
// disk's.
func streamBody(w http.ResponseWriter, tmp *os.File, body io.Reader,
	path, wantSum string) (size int64, sum string, ok bool) {
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		slog.Debug("Failed to receive file", "path", path, "bytes", size, "error", err)
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			writeFSError(w, err)
		} else {
			writeFileError(w, http.StatusBadRequest, fmt.Sprintf("reading request body: %v", err))
		}
		return 0, "", false
	}
	sum = hex.EncodeToString(h.Sum(nil))
	if wantSum != "" && sum != wantSum {
		writeFileError(w, http.StatusBadRequest, fmt.Sprintf("sha256 mismatch: got %s, expected %s", sum, wantSum))
		return size, sum, false
	}
	return size, sum, true
}

func (s *Server) putFileHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := parsePutReq(w, r)
	if !ok {
		return
	}
	dest, ok := resolvePutDest(w, req)
	if !ok {
		return
	}

	if req.parents {
		if err := os.MkdirAll(dest.dir, 0o755); err != nil {
			writeFSError(w, err)
			return
		}
	}

	// Write to a temp file next to the destination and rename it into place,
	// so readers never see a partial file and a failed upload leaves the
	// destination as it was.
	tmp, err := os.CreateTemp(dest.dir, "."+dest.name+".koyeb-tmp-*")
	if err != nil {
		slog.Debug("Failed to create temp file", "path", dest.path, "error", err)
		writeFSError(w, err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	size, sum, ok := streamBody(w, tmp, r.Body, dest.path, req.wantSum)
	if !ok {
		return
	}

	if err := commitTempFile(tmp, dest.path, dest.mode); err != nil {
		slog.Debug("Failed to commit file", "path", dest.path, "error", err)
		writeFSError(w, err)
		return
	}
	committed = true

	slog.Debug("File written", "path", dest.path, "bytes", size)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(PutFileResponse{Size: size, SHA256: sum})
}

// writeTarget follows symlinks at path, as /write_file does, rather than
// replacing the link, including a link whose target doesn't exist yet.
func writeTarget(path string) (string, error) {
	// 40 hops, the kernel's own limit: past it open fails ELOOP, so refuse
	// rather than let the rename replace the final link.
	for range 40 {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			return path, nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return path, nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return "", &fs.PathError{Op: "resolve", Path: path, Err: syscall.ELOOP}
}

// commitTempFile makes tmp durable with mode and renames it to path.
func commitTempFile(tmp *os.File, path string, mode fs.FileMode) error {
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Persist the rename itself. The file is in place either way, so a failure
	// here is only logged.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		if err := d.Sync(); err != nil {
			slog.Debug("Failed to sync directory", "path", path, "error", err)
		}
		_ = d.Close()
	}
	return nil
}

func (s *Server) statFileHandler(w http.ResponseWriter, r *http.Request) {
	path, ok := pathParam(w, r)
	if !ok {
		return
	}
	hashName := r.URL.Query().Get("hash")
	if hashName != "" && hashName != "sha256" {
		writeFileError(w, http.StatusBadRequest, "hash must be sha256")
		return
	}

	// Follows symlinks, like GET /files: this is what a download would get.
	info, err := os.Stat(path)
	if err != nil {
		writeFSError(w, err)
		return
	}
	entry := fileEntry("", info)

	if hashName != "" {
		if !info.Mode().IsRegular() {
			writeFileError(w, http.StatusBadRequest, fmt.Sprintf("%s is not a regular file", path))
			return
		}
		sum, err := sha256File(path)
		if err != nil {
			writeFSError(w, err)
			return
		}
		entry.SHA256 = sum
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entry)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) listFilesHandler(w http.ResponseWriter, r *http.Request) {
	root, ok := pathParam(w, r)
	if !ok {
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1"

	info, err := os.Stat(root)
	if err != nil {
		writeFSError(w, err)
		return
	}
	if !info.IsDir() {
		writeFileError(w, http.StatusBadRequest, fmt.Sprintf("%s is not a directory", root))
		return
	}

	// Walk the directory a symlink points to: WalkDir doesn't descend into a
	// root that is a link. Links inside it are still listed, not followed.
	walkRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		writeFSError(w, err)
		return
	}

	slog.Debug("Listing files", "path", root, "recursive", recursive)

	// Errors after the first line can't change the status, so an entry that
	// can't be read is listed with its error and the walk goes on.
	w.Header().Set("Content-Type", "application/x-ndjson")
	count, err := listWalk(w, walkRoot, recursive)
	if err != nil {
		slog.Debug("Listing stopped", "path", root, "entries", count, "error", err)
		return
	}
	slog.Debug("Files listed", "path", root, "entries", count)
}

// listWalk streams the tree under walkRoot as NDJSON, one entry per line, so a
// large tree is never held in memory. An entry that can't be read is listed
// with its error and the walk goes on; a failed write stops it.
func listWalk(w io.Writer, walkRoot string, recursive bool) (count int, err error) {
	enc := json.NewEncoder(w)
	err = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if p == walkRoot && err == nil {
			return nil
		}
		rel, relErr := filepath.Rel(walkRoot, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if err != nil {
			return enc.Encode(FileEntry{Path: rel, Error: err.Error()})
		}
		// WalkDir doesn't follow symlinks, and Info is an lstat.
		fi, err := d.Info()
		if err != nil {
			return enc.Encode(FileEntry{Path: rel, Error: err.Error()})
		}
		// An encode error is a failed write: the client is gone, stop the walk.
		if err := enc.Encode(fileEntry(rel, fi)); err != nil {
			return err
		}
		count++
		if d.IsDir() && !recursive {
			return filepath.SkipDir
		}
		return nil
	})
	return count, err
}
