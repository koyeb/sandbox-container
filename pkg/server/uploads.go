package server

import (
	"cmp"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Multipart upload sessions. A file too large for one request (the
// cloud-runtime cap is 100 MiB) is uploaded as offset-addressed parts into a
// session: POST /files/uploads opens one, PUT /files/uploads/{id}?offset=N
// writes a part, GET /files/uploads/{id} reports the received ranges,
// POST /files/uploads/{id}/complete checks coverage and renames into place,
// DELETE /files/uploads/{id} aborts. The parts land in a temp file next to
// the destination, truncated to the final size up front, so any order, any
// parallelism and any retry is a plain WriteAt. Sessions live in memory: an
// executor restart loses them, and the client starts over. A marker file per
// session names its temp file so the next run can sweep the orphans at
// startup.

const (
	// maxUploadPartSize is the largest part accepted: the 100 MiB request
	// cap of the cloud-runtime hop in front of this executor.
	maxUploadPartSize = 100 << 20
	// uploadSessionTTL is how long a session lives without a part.
	uploadSessionTTL = time.Hour
)

// uploadSpan is a half-open [Start, End) range of bytes received.
type uploadSpan struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type openUploadRequest struct {
	Path   string `json:"path"`
	Size   *int64 `json:"size"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

type openUploadResponse struct {
	UploadID    string `json:"upload_id"`
	MaxPartSize int64  `json:"max_part_size"`
}

type uploadStatusResponse struct {
	Size   int64        `json:"size"`
	Ranges []uploadSpan `json:"ranges"`
}

type completeUploadResponse struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// uploadErrorResponse is an error body that can carry a progress report.
type uploadErrorResponse struct {
	Error  string       `json:"error"`
	Ranges []uploadSpan `json:"ranges,omitempty"`
}

// uploadSession is one open multipart upload.
type uploadSession struct {
	id       string
	file     *os.File // the temp file, kept open for part writes
	tempPath string
	dest     string // symlink-resolved destination
	size     int64
	mode     fs.FileMode
	modeSet  bool
	wantSum  string // whole-file sha256, checked at complete
	spans    []uploadSpan
	lastPart time.Time
}

// uploadStore holds the live sessions. mu is taken only by the store's own
// methods and guards the map plus the mutable session fields (spans,
// lastPart); the rest of a session is immutable after open. Part file I/O
// happens outside the lock, so parts stream in parallel. The brief
// Close/Remove of terminal cleanup runs under it on the expiry and abort
// paths; once complete has retired a session the discard runs lock-free,
// with the session already off the map.
type uploadStore struct {
	mu       sync.Mutex
	sessions map[string]*uploadSession
	dir      string // marker registry, one file per live session
}

// newUploadStore prepares the marker registry, sweeps the temp files the
// previous run left behind, and starts the janitor for sessions abandoned
// without any further request.
func newUploadStore(dir string) *uploadStore {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Debug("Failed to create upload markers directory", "path", dir, "error", err)
	}
	st := &uploadStore{sessions: map[string]*uploadSession{}, dir: dir}
	st.sweepMarkers()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			st.expireSessions(time.Now())
		}
	}()
	return st
}

// sweepMarkers removes the previous run's session temp files and markers:
// sessions do not survive a restart, so every marker names an orphan.
func (st *uploadStore) sweepMarkers() {
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		slog.Debug("Failed to read upload markers directory", "path", st.dir, "error", err)
		return
	}
	for _, e := range entries {
		marker := filepath.Join(st.dir, e.Name())
		if tmp, err := os.ReadFile(marker); err == nil {
			if err := os.Remove(strings.TrimSpace(string(tmp))); err != nil {
				slog.Debug("Failed to sweep upload temp file", "path", string(tmp), "error", err)
			}
		}
		if err := os.Remove(marker); err != nil {
			slog.Debug("Failed to remove upload marker", "path", marker, "error", err)
		}
	}
}

// expireSessions removes the sessions idle past the TTL.
func (st *uploadStore) expireSessions(now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, sess := range st.sessions {
		if now.Sub(sess.lastPart) >= uploadSessionTTL {
			st.removeLocked(sess)
		}
	}
}

// session returns the live session, treating one idle past the TTL as gone.
func (st *uploadStore) session(id string) (*uploadSession, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.liveSessionLocked(id)
}

// liveSessionLocked is session() for a caller already holding mu.
func (st *uploadStore) liveSessionLocked(id string) (*uploadSession, bool) {
	sess, ok := st.sessions[id]
	if !ok {
		return nil, false
	}
	if time.Since(sess.lastPart) >= uploadSessionTTL {
		st.removeLocked(sess)
		return nil, false
	}
	return sess, true
}

// removeLocked drops sess from the store along with its temp file and marker.
func (st *uploadStore) removeLocked(sess *uploadSession) {
	delete(st.sessions, sess.id)
	st.discard(sess)
}

// discard closes and deletes a session's temp file and marker.
func (st *uploadStore) discard(sess *uploadSession) {
	sess.file.Close()
	if err := os.Remove(sess.tempPath); err != nil {
		slog.Debug("Failed to remove upload temp file", "path", sess.tempPath, "error", err)
	}
	marker := st.markerPath(sess.id)
	if err := os.Remove(marker); err != nil {
		slog.Debug("Failed to remove upload marker", "path", marker, "error", err)
	}
}

func (st *uploadStore) markerPath(id string) string {
	return filepath.Join(st.dir, id)
}

// add registers a new session.
func (st *uploadStore) add(sess *uploadSession) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions[sess.id] = sess
}

// touch refreshes the session's TTL.
func (st *uploadStore) touch(sess *uploadSession) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess.lastPart = time.Now()
}

// recordPart notes a received part's bytes and refreshes the TTL.
func (st *uploadStore) recordPart(sess *uploadSession, start, end int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess.lastPart = time.Now()
	sess.spans = addSpan(sess.spans, start, end)
}

// receivedSpans copies the ranges received so far; never nil, so callers
// can encode it directly.
func (st *uploadStore) receivedSpans(sess *uploadSession) []uploadSpan {
	st.mu.Lock()
	defer st.mu.Unlock()
	if sess.spans == nil {
		return []uploadSpan{}
	}
	return slices.Clone(sess.spans)
}

// abort removes the session named by id along with its temp file and
// marker. nil means no such session was live.
func (st *uploadStore) abort(id string) *uploadSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, ok := st.liveSessionLocked(id)
	if !ok {
		return nil
	}
	st.removeLocked(sess)
	return sess
}

// claimForCompletion retires the session when its parts cover [0, size),
// returning it for the caller to hash and commit. A non-empty missing means
// the session stays live and resumable.
func (st *uploadStore) claimForCompletion(id string) (sess *uploadSession, missing []uploadSpan, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, ok = st.liveSessionLocked(id)
	if !ok {
		return nil, nil, false
	}
	missing = missingSpans(sess.spans, sess.size)
	if len(missing) != 0 {
		return sess, missing, true
	}
	delete(st.sessions, sess.id)
	return sess, nil, true
}

// addSpan merges [start, end) into the sorted, coalesced spans.
func addSpan(spans []uploadSpan, start, end int64) []uploadSpan {
	ns, ne := start, end
	out := make([]uploadSpan, 0, len(spans)+1)
	for _, sp := range spans {
		if sp.End >= ns && ne >= sp.Start { // overlapping or adjacent
			ns, ne = min(ns, sp.Start), max(ne, sp.End)
			continue
		}
		out = append(out, sp)
	}
	out = append(out, uploadSpan{ns, ne})
	slices.SortFunc(out, func(a, b uploadSpan) int { return cmp.Compare(a.Start, b.Start) })
	return out
}

// missingSpans returns the parts of [0, size) that no span covers.
func missingSpans(spans []uploadSpan, size int64) []uploadSpan {
	var missing []uploadSpan
	at := int64(0)
	for _, sp := range spans {
		if sp.Start > at {
			missing = append(missing, uploadSpan{at, sp.Start})
		}
		at = max(at, sp.End)
	}
	if at < size {
		missing = append(missing, uploadSpan{at, size})
	}
	return missing
}

// parseUploadMode parses an optional octal mode string ("0644").
func parseUploadMode(s string) (fs.FileMode, bool, error) {
	if s == "" {
		return 0o644, false, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o777 {
		return 0, false, errors.New("mode must be octal permission bits, e.g. 0644")
	}
	return fs.FileMode(v), true, nil
}

// parseHexSum validates an optional lowercase sha256 hex string.
func parseHexSum(s string) (string, error) {
	s = strings.ToLower(s)
	if s == "" {
		return "", nil
	}
	if b, err := hex.DecodeString(s); err != nil || len(b) != sha256.Size {
		return "", errors.New("sha256 must be 64 hex characters")
	}
	return s, nil
}

func (s *Server) registerUploadRoutes(mux *http.ServeMux) {
	if s.uploads == nil {
		s.uploads = newUploadStore(filepath.Join(os.TempDir(), "koyeb-file-uploads"))
	}
	mux.Handle("POST /files/uploads", s.authMiddleware(http.HandlerFunc(s.openUploadHandler)))
	mux.Handle("PUT /files/uploads/{id}", s.authMiddleware(http.HandlerFunc(s.putUploadPartHandler)))
	mux.Handle("GET /files/uploads/{id}", s.authMiddleware(http.HandlerFunc(s.uploadStatusHandler)))
	mux.Handle("POST /files/uploads/{id}/complete", s.authMiddleware(http.HandlerFunc(s.completeUploadHandler)))
	mux.Handle("DELETE /files/uploads/{id}", s.authMiddleware(http.HandlerFunc(s.abortUploadHandler)))
}

// openUploadParams is a validated POST /files/uploads request.
type openUploadParams struct {
	path    string
	size    int64
	mode    fs.FileMode
	modeSet bool
	wantSum string
}

// parseOpenUpload decodes and validates the POST /files/uploads body.
func parseOpenUpload(w http.ResponseWriter, r *http.Request) (openUploadParams, error) {
	var req openUploadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		return openUploadParams{}, errors.New("invalid request body: " + err.Error())
	}
	if req.Path == "" {
		return openUploadParams{}, errors.New("path is required")
	}
	if req.Size == nil || *req.Size < 0 {
		return openUploadParams{}, errors.New("size is required and must be a non-negative integer")
	}
	mode, modeSet, err := parseUploadMode(req.Mode)
	if err != nil {
		return openUploadParams{}, err
	}
	wantSum, err := parseHexSum(req.SHA256)
	if err != nil {
		return openUploadParams{}, err
	}
	return openUploadParams{path: req.Path, size: *req.Size, mode: mode, modeSet: modeSet, wantSum: wantSum}, nil
}

// createUploadSession resolves the destination and lays down its temp file,
// truncated to the final size so parts can be written at their offsets in
// any order.
func createUploadSession(p openUploadParams) (*uploadSession, error) {
	dest, err := writeTarget(filepath.Clean(p.path))
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		return nil, &fs.PathError{Op: "upload", Path: dest, Err: errDestIsDir}
	}
	dir, name := filepath.Split(dest)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+name+tempMarker+"*")
	if err != nil {
		return nil, err
	}
	if err := tmp.Truncate(p.size); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	return &uploadSession{
		id:       uuid.New().String(),
		file:     tmp,
		tempPath: tmp.Name(),
		dest:     dest,
		size:     p.size,
		mode:     p.mode,
		modeSet:  p.modeSet,
		wantSum:  p.wantSum,
		lastPart: time.Now(),
	}, nil
}

// openUploadHandler opens a session and answers its id and the part cap.
func (s *Server) openUploadHandler(w http.ResponseWriter, r *http.Request) {
	p, err := parseOpenUpload(w, r)
	if err != nil {
		writeFileError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := createUploadSession(p)
	if err != nil {
		slog.Debug("Failed to open upload session", "path", p.path, "error", err)
		writeFSError(w, err)
		return
	}

	// The marker outlives this process just long enough for the next run's
	// startup sweep to find the temp file. A session that cannot be
	// recorded for that sweep is refused: it would leave an orphan no
	// future run can find.
	if err := os.WriteFile(s.uploads.markerPath(sess.id), []byte(sess.tempPath), 0o600); err != nil {
		slog.Debug("Failed to write upload marker", "path", sess.tempPath, "error", err)
		s.uploads.discard(sess)
		writeFileError(w, http.StatusInternalServerError, "failed to record the upload session")
		return
	}
	s.uploads.add(sess)

	slog.Debug("Upload session opened", "path", sess.dest, "bytes", sess.size)
	writeJSON(w, http.StatusOK, openUploadResponse{UploadID: sess.id, MaxPartSize: maxUploadPartSize})
}

// errPartTooLong means a part streamed past its cap.
var errPartTooLong = errors.New("part is longer than allowed")

// partWriter writes at a fixed offset of the session's temp file, capped at
// limit bytes: a longer part is refused rather than written past the part
// cap or past the end of the upload. Every chunk that lands refreshes the
// session's TTL, so a part streaming for longer than the TTL never has its
// session expired mid-copy.
type partWriter struct {
	st    *uploadStore
	sess  *uploadSession
	file  *os.File
	off   int64
	limit int64
}

func (pw *partWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > pw.limit {
		return 0, errPartTooLong
	}
	n, err := pw.file.WriteAt(p, pw.off)
	pw.off += int64(n)
	pw.limit -= int64(n)
	if n > 0 {
		pw.st.touch(pw.sess)
	}
	return n, err
}

// uploadPartParams validates a part's offset, declared length and checksum
// header, answering the client itself on a bad one. A declared length is
// checked before a byte is read; the copy cap in partWriter catches bodies
// that lied or stream without one.
func uploadPartParams(w http.ResponseWriter, r *http.Request, sess *uploadSession) (offset int64, wantSum string, ok bool) {
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeFileError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return 0, "", false
	}
	if r.ContentLength > 0 {
		if r.ContentLength > maxUploadPartSize {
			writePartTooLarge(w)
			return 0, "", false
		}
		if offset+r.ContentLength > sess.size {
			writePartPastEnd(w)
			return 0, "", false
		}
	}
	wantSum, err = parseHexSum(r.Header.Get("X-Content-SHA256"))
	if err != nil {
		writeFileError(w, http.StatusBadRequest, err.Error())
		return 0, "", false
	}
	return offset, wantSum, true
}

// writeUnknownUpload answers a request naming no live session.
func writeUnknownUpload(w http.ResponseWriter) {
	writeFileError(w, http.StatusNotFound, "unknown upload session")
}

// writePartTooLarge answers a part above max_part_size.
func writePartTooLarge(w http.ResponseWriter) {
	writeFileError(w, http.StatusRequestEntityTooLarge,
		fmt.Sprintf("part is larger than max_part_size (%d bytes)", maxUploadPartSize))
}

// writePartPastEnd answers a part that would write past the upload's end.
func writePartPastEnd(w http.ResponseWriter) {
	writeFileError(w, http.StatusBadRequest, "part writes past the end of the upload")
}

// writeSumMismatch answers a checksum that did not match the bytes.
func writeSumMismatch(w http.ResponseWriter, got, want string) {
	writeFileError(w, http.StatusBadRequest, fmt.Sprintf("sha256 mismatch: got %s, expected %s", got, want))
}

// writePartError classifies a failed part receive: a body over the part cap
// or past the upload's end was refused by partWriter; a read error means the
// client went away, a write error is the disk's.
func writePartError(w http.ResponseWriter, err error, sess *uploadSession, offset int64) {
	if errors.Is(err, errPartTooLong) {
		if sess.size-offset > maxUploadPartSize {
			writePartTooLarge(w)
		} else {
			writePartPastEnd(w)
		}
		return
	}
	writeReceiveError(w, err)
}

// putUploadPartHandler streams one part to its offset. Parts may arrive in
// any order and in parallel; a retried part overwrites the same bytes. Each
// request holds a single 32 KiB io.Copy buffer.
func (s *Server) putUploadPartHandler(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.uploads.session(r.PathValue("id"))
	if !ok {
		writeUnknownUpload(w)
		return
	}
	offset, wantSum, ok := uploadPartParams(w, r, sess)
	if !ok {
		return
	}

	// The part refreshes the TTL from its first byte, and every chunk that
	// lands keeps it fresh: a slow client cannot have its session expired
	// mid-copy, however long the copy runs.
	s.uploads.touch(sess)

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, &partWriter{st: s.uploads, sess: sess, file: sess.file, off: offset, limit: min(maxUploadPartSize, sess.size-offset)}), r.Body)
	if err != nil {
		slog.Debug("Failed to receive part", "path", sess.dest, "offset", offset, "error", err)
		writePartError(w, err, sess, offset)
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSum != "" && sum != wantSum {
		writeSumMismatch(w, sum, wantSum)
		return
	}
	if n == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.uploads.recordPart(sess, offset, offset+n)
	w.WriteHeader(http.StatusNoContent)
}

// uploadStatusHandler answers the ranges received so far, so a client can
// resume.
func (s *Server) uploadStatusHandler(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.uploads.session(r.PathValue("id"))
	if !ok {
		writeUnknownUpload(w)
		return
	}
	spans := s.uploads.receivedSpans(sess)
	writeJSON(w, http.StatusOK, uploadStatusResponse{Size: sess.size, Ranges: spans})
}

// verifyUploadSum hashes the temp file when the session promised a sha256.
// A mismatch sweeps the corrupted session and answers the client itself.
func (s *Server) verifyUploadSum(w http.ResponseWriter, sess *uploadSession) (string, bool) {
	if sess.wantSum == "" {
		return "", true
	}
	sum, err := sha256File(sess.tempPath)
	if err != nil {
		slog.Debug("Failed to hash upload", "path", sess.dest, "error", err)
		s.uploads.discard(sess)
		writeFSError(w, err)
		return "", false
	}
	if sum != sess.wantSum {
		slog.Debug("Upload checksum mismatch", "path", sess.dest, "got", sum, "expected", sess.wantSum)
		s.uploads.discard(sess)
		writeSumMismatch(w, sum, sess.wantSum)
		return "", false
	}
	return sum, true
}

// effectiveUploadMode returns the mode to commit with: an explicit one, an
// existing destination's carried over, 0644 for a new file — like PUT
// /files without mode=.
func effectiveUploadMode(sess *uploadSession) fs.FileMode {
	if sess.modeSet {
		return sess.mode
	}
	if info, err := os.Stat(sess.dest); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}

// completeUploadHandler checks that the parts cover [0, size), hashes when
// the session promised a sha256, then fsyncs and renames the temp file onto
// the destination.
func (s *Server) completeUploadHandler(w http.ResponseWriter, r *http.Request) {
	st := s.uploads
	sess, missing, ok := st.claimForCompletion(r.PathValue("id"))
	if !ok {
		writeUnknownUpload(w)
		return
	}
	if missing != nil {
		writeJSON(w, http.StatusConflict, uploadErrorResponse{
			Error:  fmt.Sprintf("%d range(s) of the upload are still missing", len(missing)),
			Ranges: missing,
		})
		return
	}
	// The session is retired: a new part for it now 404s, and the slow
	// hash-and-fsync below cannot race a rename. A part already in flight
	// keeps its fd: a retry of already-recorded bytes just overwrites the
	// same ones, but on a failed complete the session's file is closed
	// under it and that straggler gets a 500. Both are client-race corners
	// the SDK's sequential flow never hits.

	sum, ok := s.verifyUploadSum(w, sess)
	if !ok {
		return
	}
	if err := commitTempFile(sess.file, sess.dest, effectiveUploadMode(sess)); err != nil {
		slog.Debug("Failed to commit upload", "path", sess.dest, "error", err)
		st.discard(sess)
		writeFSError(w, err)
		return
	}
	if err := os.Remove(st.markerPath(sess.id)); err != nil {
		slog.Debug("Failed to remove upload marker", "path", st.markerPath(sess.id), "error", err)
	}

	slog.Debug("Upload completed", "path", sess.dest, "bytes", sess.size)
	writeJSON(w, http.StatusOK, completeUploadResponse{Size: sess.size, SHA256: sum})
}

// abortUploadHandler aborts the session and removes its temp file.
func (s *Server) abortUploadHandler(w http.ResponseWriter, r *http.Request) {
	sess := s.uploads.abort(r.PathValue("id"))
	if sess == nil {
		writeUnknownUpload(w)
		return
	}
	slog.Debug("Upload aborted", "path", sess.dest)
	w.WriteHeader(http.StatusNoContent)
}
