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

func (s *Server) registerFileRoutes(mux *http.ServeMux) {
	// A GET pattern also matches HEAD.
	mux.Handle("GET /files", s.authMiddleware(http.HandlerFunc(s.getFileHandler)))
	mux.Handle("PUT /files", s.authMiddleware(http.HandlerFunc(s.putFileHandler)))
	mux.Handle("GET /files/stat", s.authMiddleware(http.HandlerFunc(s.statFileHandler)))
	mux.Handle("GET /files/list", s.authMiddleware(http.HandlerFunc(s.listFilesHandler)))
	s.registerUploadRoutes(mux)
}

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
	case errors.Is(err, errDestIsDir), errors.Is(err, fs.ErrExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.ELOOP):
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

// notRegularError reports a download target that is a FIFO, socket, device
// or directory: open() on a writer-less FIFO would block until a writer
// appears, holding the handler and its connection forever.
type notRegularError struct {
	path string
	kind string
}

func (e notRegularError) Error() string {
	return fmt.Sprintf("%s is not a regular file (it is a %s)", e.path, e.kind)
}

// openRegular opens path for download, following symlinks like the download
// itself does, and returns the opened file with its Stat. A special file is
// rejected before the open; a race that swaps the file for a special one
// between Stat and Open is caught after it.
func openRegular(path string) (*os.File, fs.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, notRegularError{path: path, kind: fileType(info.Mode())}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if info, err = f.Stat(); err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, notRegularError{path: path, kind: fileType(info.Mode())}
	}
	return f, info, nil
}

func (s *Server) getFileHandler(w http.ResponseWriter, r *http.Request) {
	path, ok := pathParam(w, r)
	if !ok {
		return
	}

	f, info, err := openRegular(path)
	if err != nil {
		var nre notRegularError
		if errors.As(err, &nre) {
			writeFileError(w, http.StatusBadRequest, nre.Error())
		} else {
			slog.Debug("Failed to open file", "path", path, "error", err)
			writeFSError(w, err)
		}
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

// errDestIsDir marks a PUT whose resolved destination is a directory.
var errDestIsDir = errors.New("destination is a directory")

func (s *Server) putFileHandler(w http.ResponseWriter, r *http.Request) {
	path, ok := pathParam(w, r)
	if !ok {
		return
	}
	pp, err := parsePutParams(r)
	if err != nil {
		writeFileError(w, http.StatusBadRequest, err.Error())
		return
	}

	dir, name, mode, err := resolvePutTarget(path, pp)
	if err != nil {
		slog.Debug("Failed to prepare destination", "path", path, "error", err)
		writeFSError(w, err)
		return
	}

	// Write to a temp file next to the destination and rename it into place,
	// so readers never see a partial file and a failed upload leaves the
	// destination as it was.
	tmp, size, sum, err := receiveToTemp(r, dir, name)
	if err != nil {
		slog.Debug("Failed to receive file", "path", path, "error", err)
		writeReceiveError(w, err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	if pp.wantSum != "" && sum != pp.wantSum {
		writeFileError(w, http.StatusBadRequest, fmt.Sprintf("sha256 mismatch: got %s, expected %s", sum, pp.wantSum))
		return
	}

	if err := commitTempFile(tmp, filepath.Join(dir, name), mode); err != nil {
		slog.Debug("Failed to commit file", "path", path, "error", err)
		writeFSError(w, err)
		return
	}
	committed = true

	slog.Debug("File written", "path", path, "bytes", size)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(PutFileResponse{Size: size, SHA256: sum})
}

// putParams carries a PUT's optional parameters from their parsing to the
// destination resolution.
type putParams struct {
	mode    fs.FileMode
	modeSet bool
	parents bool
	wantSum string
}

// parsePutParams validates the optional mode, parents and X-Content-SHA256
// of a PUT. mode is the default 0644 unless mode= was given.
func parsePutParams(r *http.Request) (putParams, error) {
	p := putParams{mode: 0o644, parents: r.URL.Query().Get("parents") == "1"}
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil || v > 0o777 {
			return putParams{}, errors.New("mode must be octal permission bits, e.g. 0644")
		}
		p.mode, p.modeSet = fs.FileMode(v), true
	}
	p.wantSum = strings.ToLower(r.Header.Get("X-Content-SHA256"))
	if p.wantSum != "" {
		if b, err := hex.DecodeString(p.wantSum); err != nil || len(b) != sha256.Size {
			return putParams{}, errors.New("X-Content-SHA256 must be 64 hex characters")
		}
	}
	return p, nil
}

// resolvePutTarget follows symlinks at path (as /write_file does), carries an
// existing destination's mode over when mode= wasn't given, and creates the
// parent directories when parents was requested. It returns the directory to
// create the temp file in, the destination's file name, and the mode to commit
// with.
func resolvePutTarget(path string, pp putParams) (dir, name string, effMode fs.FileMode, err error) {
	path, err = writeTarget(path)
	if err != nil {
		return "", "", 0, err
	}
	effMode = pp.mode
	if info, statErr := os.Stat(path); statErr == nil {
		if info.IsDir() {
			return "", "", 0, &fs.PathError{Op: "put", Path: path, Err: errDestIsDir}
		}
		if !pp.modeSet {
			effMode = info.Mode().Perm()
		}
	}
	dir, name = filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if pp.parents {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", 0, err
		}
	}
	return dir, name, effMode, nil
}

// writeReceiveError classifies a failed receive: a read error means the
// client went away or sent a short body; a write error is the disk's.
func writeReceiveError(w http.ResponseWriter, err error) {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		writeFSError(w, err)
	} else {
		writeFileError(w, http.StatusBadRequest, fmt.Sprintf("reading request body: %v", err))
	}
}

// receiveToTemp streams the request body into a new temp file in dir,
// hashing it as it goes. An error that is not an *fs.PathError means the
// body read failed, not the disk.
func receiveToTemp(r *http.Request, dir, name string) (*os.File, int64, string, error) {
	tmp, err := os.CreateTemp(dir, "."+name+".koyeb-tmp-*")
	if err != nil {
		return nil, 0, "", err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r.Body)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, "", err
	}
	return tmp, size, hex.EncodeToString(h.Sum(nil)), nil
}

// writeTarget follows symlinks at path, as /write_file does, rather than
// replacing the link, including a link whose target doesn't exist yet. A
// chain longer than the kernel's limit resolves to no file: fail the way
// the kernel would, instead of overwriting the last link.
func writeTarget(path string) (string, error) {
	// 40 hops, the kernel's limit.
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
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return "", &fs.PathError{Op: "write", Path: path, Err: syscall.ELOOP}
	}
	return path, nil
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
		d.Close()
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

	// One JSON object per line, written as the walk goes: a large tree is never
	// held in memory. Errors after the first line can't change the status, so
	// an entry that can't be read is listed with its error and the walk goes on.
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	count := 0
	err = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		emitted, walkErr := encodeListEntry(enc, walkRoot, p, d, err, recursive)
		if emitted {
			count++
		}
		return walkErr
	})
	if err != nil {
		slog.Debug("Listing stopped", "path", root, "entries", count, "error", err)
		return
	}
	slog.Debug("Files listed", "path", root, "entries", count)
}

// encodeListEntry emits one NDJSON line for a walk entry and returns the
// value the WalkDir callback should return. emitted reports whether a
// regular entry was written. WalkDir doesn't follow symlinks, and Info is
// an lstat.
func encodeListEntry(enc *json.Encoder, walkRoot, p string, d fs.DirEntry, walkErr error, recursive bool) (emitted bool, err error) {
	if p == walkRoot && walkErr == nil {
		return false, nil
	}
	rel, relErr := filepath.Rel(walkRoot, p)
	if relErr != nil {
		return false, relErr
	}
	rel = filepath.ToSlash(rel)
	if walkErr != nil {
		return false, enc.Encode(FileEntry{Path: rel, Error: walkErr.Error()})
	}
	fi, err := d.Info()
	if err != nil {
		return false, enc.Encode(FileEntry{Path: rel, Error: err.Error()})
	}
	// An encode error is a failed write: the client is gone, stop the walk.
	if err := enc.Encode(fileEntry(rel, fi)); err != nil {
		return false, err
	}
	if d.IsDir() && !recursive {
		return true, filepath.SkipDir
	}
	return true, nil
}
