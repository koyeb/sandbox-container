package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func filesURL(route, path string, params ...string) string {
	q := url.Values{"path": {path}}
	for i := 0; i+1 < len(params); i += 2 {
		q.Set(params[i], params[i+1])
	}
	return route + "?" + q.Encode()
}

func serve(mux http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func putFile(t *testing.T, mux http.Handler, path string, body []byte, params ...string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(mux, newAuthRequest(http.MethodPut, filesURL("/files", path, params...), body))
}

func expectError(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("expected %d, got %d: %s", status, w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp["error"] == "" {
		t.Fatalf("expected a JSON error body, got %q (%v)", w.Body.String(), err)
	}
}

// listTempFiles returns the leftover temp files of PUT /files in dir.
func listTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".*.koyeb-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestFilesBinaryRoundTrip(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "all-bytes.bin")

	content := make([]byte, 0, 256*4)
	for range 4 {
		for b := range 256 {
			content = append(content, byte(b))
		}
	}
	sum := sha256.Sum256(content)

	w := putFile(t, mux, path, content)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var put PutFileResponse
	if err := json.NewDecoder(w.Body).Decode(&put); err != nil {
		t.Fatal(err)
	}
	if put.Size != int64(len(content)) || put.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("PUT response %+v, expected size %d sha256 %x", put, len(content), sum)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, content) {
		t.Fatal("bytes on disk differ from the bytes sent")
	}

	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", path), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatal("GET returned different bytes")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	if len(listTempFiles(t, filepath.Dir(path))) != 0 {
		t.Error("temp file left behind")
	}
}

func TestFilesPutChecksumAndMode(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "script.sh")
	content := []byte("#!/bin/sh\necho hi\n")
	sum := sha256.Sum256(content)

	req := newAuthRequest(http.MethodPut, filesURL("/files", path, "parents", "1", "mode", "0755"), content)
	req.Header.Set("X-Content-SHA256", strings.ToUpper(hex.EncodeToString(sum[:])))
	if w := serve(mux, req); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode %o, expected 0755", info.Mode().Perm())
	}

	// An overwrite without mode keeps the file's mode.
	if w := putFile(t, mux, path, []byte("new")); w.Code != http.StatusOK {
		t.Fatalf("overwrite: expected 200, got %d", w.Code)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o755 {
		t.Errorf("overwrite changed mode to %o", info.Mode().Perm())
	}

	// A checksum mismatch leaves the destination as it was.
	req = newAuthRequest(http.MethodPut, filesURL("/files", path), []byte("corrupted"))
	req.Header.Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
	expectError(t, serve(mux, req), http.StatusBadRequest)
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("destination changed after a checksum mismatch: %q", got)
	}
	if len(listTempFiles(t, filepath.Dir(path))) != 0 {
		t.Error("temp file left behind")
	}
}

// A dangling link is written through too: its target is created, as os.WriteFile does.
func TestFilesPutDanglingSymlink(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	link := filepath.Join(dir, "link")
	if err := os.Symlink("sub/target", link); err != nil {
		t.Fatal(err)
	}

	if w := putFile(t, mux, link, []byte("new")); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sub", "target")); string(got) != "new" {
		t.Errorf("target holds %q", got)
	}
}

func TestFilesPutThroughSymlink(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	os.WriteFile(target, []byte("old"), 0o600)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if w := putFile(t, mux, link, []byte("new")); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("target holds %q", got)
	}
}

// failingReader returns data, then err, like a client that goes away mid-upload.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestFilesPutInterrupted(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	os.WriteFile(existing, []byte("keep me"), 0o644)
	fresh := filepath.Join(dir, "fresh")

	for _, path := range []string{existing, fresh} {
		body := &failingReader{data: bytes.Repeat([]byte("x"), 1<<20), err: io.ErrUnexpectedEOF}
		req := httptest.NewRequest(http.MethodPut, filesURL("/files", path), body)
		req.Header.Set("Authorization", "Bearer test-secret")
		expectError(t, serve(mux, req), http.StatusBadRequest)
	}

	if got, _ := os.ReadFile(existing); string(got) != "keep me" {
		t.Errorf("existing file changed: %q", got)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("interrupted PUT created the destination: %v", err)
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
}

func TestFilesRange(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "f")
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	os.WriteFile(path, content, 0o644)

	head := serve(mux, newAuthRequest(http.MethodHead, filesURL("/files", path), nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d with %d body bytes", head.Code, head.Body.Len())
	}
	etag := head.Header().Get("ETag")
	if etag == "" || strings.HasPrefix(etag, "W/") {
		t.Fatalf("expected a strong ETag, got %q", etag)
	}
	if head.Header().Get("Content-Length") != "36" || head.Header().Get("Last-Modified") == "" {
		t.Fatalf("HEAD headers: %v", head.Header())
	}

	get := func(headers ...string) *httptest.ResponseRecorder {
		req := newAuthRequest(http.MethodGet, filesURL("/files", path), nil)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		return serve(mux, req)
	}

	w := get("Range", "bytes=10-19")
	if w.Code != http.StatusPartialContent || w.Body.String() != "abcdefghij" {
		t.Fatalf("Range: %d %q", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 10-19/36" {
		t.Errorf("Content-Range %q", cr)
	}

	// Resuming a download: If-Range with the current ETag gets the rest.
	w = get("Range", "bytes=30-", "If-Range", etag)
	if w.Code != http.StatusPartialContent || w.Body.String() != "uvwxyz" {
		t.Fatalf("If-Range current: %d %q", w.Code, w.Body.String())
	}

	// A stale ETag gets the whole file again.
	w = get("Range", "bytes=30-", "If-Range", `"stale"`)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("If-Range stale: %d %q", w.Code, w.Body.String())
	}

	if w = get("Range", "bytes=100-"); w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("range past the end: %d", w.Code)
	}
}

func TestFilesGetEmptyFile(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(path, nil, 0o644)

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", path), nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("expected an empty 200, got %d %q", w.Code, w.Body.String())
	}
}

func TestFilesStat(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	os.WriteFile(path, []byte("hello"), 0o640)

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/stat", path, "hash", "sha256"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entry FileEntry
	json.NewDecoder(w.Body).Decode(&entry)
	sum := sha256.Sum256([]byte("hello"))
	if entry.Type != "file" || entry.Size != 5 || entry.Mode != "0640" || entry.MTime == "" || entry.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("unexpected stat: %+v", entry)
	}

	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/stat", dir), nil))
	json.NewDecoder(w.Body).Decode(&entry)
	if w.Code != http.StatusOK || entry.Type != "dir" {
		t.Errorf("stat of a directory: %d %+v", w.Code, entry)
	}
}

func readNDJSON(t *testing.T, body io.Reader) map[string]FileEntry {
	t.Helper()
	entries := map[string]FileEntry{}
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		var e FileEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		entries[e.Path] = e
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the listing stopped early: %v", err)
	}
	return entries
}

func TestFilesList(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub", "deeper"), 0o755)
	os.WriteFile(filepath.Join(dir, "top.txt"), []byte("12345"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "deeper", "leaf"), []byte("x"), 0o600)
	// The link points at a directory: it must be listed, not followed.
	os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "link"))

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/list", dir), nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("list: %d %v", w.Code, w.Header())
	}
	flat := readNDJSON(t, w.Body)
	if len(flat) != 3 || flat["top.txt"].Size != 5 || flat["sub"].Type != "dir" || flat["link"].Type != "symlink" {
		t.Errorf("non-recursive list: %+v", flat)
	}

	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/list", dir, "recursive", "1"), nil))
	all := readNDJSON(t, w.Body)
	if len(all) != 5 || all["sub/deeper/leaf"].Mode != "0600" {
		t.Errorf("recursive list: %+v", all)
	}
	if _, ok := all["link/deeper"]; ok {
		t.Error("the walk followed a symlink")
	}

	// A root that is a link to a directory lists the directory.
	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/list", filepath.Join(dir, "link")), nil))
	viaLink := readNDJSON(t, w.Body)
	if w.Code != http.StatusOK || len(viaLink) != 1 || viaLink["deeper"].Type != "dir" {
		t.Errorf("list through a symlinked root: %d %+v", w.Code, viaLink)
	}
}

func TestFilesErrors(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, []byte("x"), 0o644)
	missing := filepath.Join(dir, "missing")

	get := func(route, path string, params ...string) *httptest.ResponseRecorder {
		return serve(mux, newAuthRequest(http.MethodGet, filesURL(route, path, params...), nil))
	}

	cases := []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
	}{
		{"GET missing", get("/files", missing), http.StatusNotFound},
		{"stat missing", get("/files/stat", missing), http.StatusNotFound},
		{"list missing", get("/files/list", missing), http.StatusNotFound},
		{"PUT missing parent", putFile(t, mux, filepath.Join(missing, "f"), []byte("x")), http.StatusNotFound},

		{"PUT onto a directory", putFile(t, mux, dir, []byte("x")), http.StatusConflict},
		{"PUT under a file", putFile(t, mux, filepath.Join(file, "f"), []byte("x")), http.StatusConflict},
		{"PUT parents under a file", putFile(t, mux, filepath.Join(file, "a", "f"), []byte("x"), "parents", "1"), http.StatusConflict},

		{"GET no path", serve(mux, newAuthRequest(http.MethodGet, "/files", nil)), http.StatusBadRequest},
		{"PUT no path", serve(mux, newAuthRequest(http.MethodPut, "/files", nil)), http.StatusBadRequest},
		{"GET a directory", get("/files", dir), http.StatusBadRequest},
		{"list a file", get("/files/list", file), http.StatusBadRequest},
		{"stat bad hash", get("/files/stat", file, "hash", "md5"), http.StatusBadRequest},
		{"PUT bad mode", putFile(t, mux, file, []byte("x"), "mode", "rwx"), http.StatusBadRequest},
		{"PUT mode out of range", putFile(t, mux, file, []byte("x"), "mode", "4755"), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { expectError(t, c.w, c.status) })
	}

	req := newAuthRequest(http.MethodPut, filesURL("/files", file), []byte("x"))
	req.Header.Set("X-Content-SHA256", "not-hex")
	expectError(t, serve(mux, req), http.StatusBadRequest)

	// Without a token the routes are closed like the others.
	req = httptest.NewRequest(http.MethodGet, filesURL("/files", file), nil)
	if w := serve(mux, req); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET: %d", w.Code)
	}
	if w := serve(mux, newAuthRequest(http.MethodPost, filesURL("/files", file), nil)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /files: %d", w.Code)
	}
}

// GET on a writer-less FIFO must return the documented 400, not block in
// open() holding the handler and its connection forever.
func TestFilesGetFIFO(t *testing.T) {
	_, mux := newTestServer(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	req := newAuthRequest(http.MethodGet, filesURL("/files", fifo), nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(mux, req) }()
	select {
	case w := <-done:
		expectError(t, w, http.StatusBadRequest)
	case <-time.After(3 * time.Second):
		t.Fatal("GET on a FIFO did not return; the handler blocked in open()")
	}
}

// GET on a unix socket path is rejected before the open, like other special
// files.
func TestFilesGetSocket(t *testing.T) {
	_, mux := newTestServer(t)
	sock := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer ln.Close()
	expectError(t, serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", sock), nil)), http.StatusBadRequest)
}

// A symlink chain longer than the kernel's limit fails the way the kernel
// would, instead of silently overwriting one of the links.
func TestFilesPutSymlinkChainTooLong(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "file")

	// 41 links: resolving the outermost is one hop too many.
	tooLong := target
	for i := 1; i <= 41; i++ {
		p := filepath.Join(dir, fmt.Sprintf("a%d", i))
		if err := os.Symlink(tooLong, p); err != nil {
			t.Fatal(err)
		}
		tooLong = p
	}
	expectError(t, putFile(t, mux, tooLong, []byte("new")), http.StatusConflict)
	if fi, err := os.Lstat(filepath.Join(dir, "a1")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the last link of the chain was replaced")
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}

	// A chain within the limit still writes through to the target — also
	// when the target already exists, so the chain resolves to a real file.
	os.WriteFile(target, []byte("old"), 0o644)
	within := target
	for i := 1; i <= 40; i++ {
		p := filepath.Join(dir, fmt.Sprintf("b%d", i))
		if err := os.Symlink(within, p); err != nil {
			t.Fatal(err)
		}
		within = p
	}
	if w := putFile(t, mux, within, []byte("ok")); w.Code != http.StatusOK {
		t.Fatalf("40-hop chain: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got, _ := os.ReadFile(target); string(got) != "ok" {
		t.Errorf("target holds %q", got)
	}
}

// mode= accepts the full range of permission bits, including both edges.
func TestFilesPutModeBoundaries(t *testing.T) {
	_, mux := newTestServer(t)
	for _, m := range []string{"0777", "0000"} {
		path := filepath.Join(t.TempDir(), "f")
		if w := putFile(t, mux, path, []byte("x"), "mode", m); w.Code != http.StatusOK {
			t.Fatalf("mode=%s: expected 200, got %d: %s", m, w.Code, w.Body.String())
		}
		want, _ := strconv.ParseUint(m, 8, 32)
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != fs.FileMode(want) {
			t.Errorf("mode=%s gave %o", m, fi.Mode().Perm())
		}
	}
}

// mode= is honored even when the destination already exists: the file's
// current mode doesn't override it.
func TestFilesPutModeOnOverwrite(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("first"), 0o644)

	if w := putFile(t, mux, path, []byte("second"), "mode", "0600"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode on overwrite: %v %o", err, fi.Mode().Perm())
	}
}

// A file that reports size 0 but has content (a Linux /proc file) is
// streamed to the end, without a Content-Length or ETag.
func TestFilesGetPseudoFileContent(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc pseudo-files on this system")
	}
	_, mux := newTestServer(t)
	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", "/proc/self/status"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /proc/self/status: %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("the pseudo-file streamed no content")
	}
	if w.Header().Get("Content-Length") != "" || w.Header().Get("ETag") != "" {
		t.Errorf("the pseudo-file path set validators: %v", w.Header())
	}
}

// A checksum header that is hex but not 64 characters is rejected before
// anything is written.
func TestFilesPutBadChecksumLength(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "f")
	req := newAuthRequest(http.MethodPut, filesURL("/files", path), []byte("x"))
	req.Header.Set("X-Content-SHA256", "deadbeef")
	expectError(t, serve(mux, req), http.StatusBadRequest)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("the file was written despite the bad checksum")
	}
}

// A file that reports size 0 is streamed without a length or validators:
// no ETag, Content-Length or Last-Modified, and HEAD writes no body.
func TestFilesGetPseudoFileHeaders(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(path, nil, 0o644)

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", path), nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("GET empty: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("ETag") != "" || w.Header().Get("Content-Length") != "" || w.Header().Get("Last-Modified") != "" {
		t.Errorf("the size-0 path set validators: %v", w.Header())
	}

	head := serve(mux, newAuthRequest(http.MethodHead, filesURL("/files", path), nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD empty: %d %q", head.Code, head.Body.String())
	}
}

// An entry the walk cannot read is listed with its error — path, a zero
// size, no type — and the walk goes on.
func TestFilesListErrorEntry(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "top.txt"), []byte("x"), 0o644)
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("y"), 0o644)
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/list", dir, "recursive", "1"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	body := w.Body.Bytes()
	entries := readNDJSON(t, bytes.NewReader(body))
	if entries["top.txt"].Type != "file" {
		t.Errorf("top.txt missing from the listing: %+v", entries)
	}

	sawError := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		if !strings.Contains(sc.Text(), `"error"`) {
			continue
		}
		sawError = true
		var e FileEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad error entry %q: %v", sc.Text(), err)
		}
		if e.Path != "sub" || e.Error == "" || e.Size != 0 || e.Type != "" {
			t.Errorf("error entry shape: %+v", e)
		}
	}
	if !sawError {
		t.Errorf("no error entry in the listing:\n%s", w.Body.String())
	}
}

// stat reports special files as type "other": it stats them, and never
// follows them into a blocking read.
func TestFilesStatSpecial(t *testing.T) {
	_, mux := newTestServer(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	var entry FileEntry
	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/stat", fifo), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("stat FIFO: %d %s", w.Code, w.Body.String())
	}
	if err := json.NewDecoder(w.Body).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry.Type != "other" {
		t.Errorf("stat FIFO type %q, expected other", entry.Type)
	}
}

// fsErrorStatus is the wire contract the SDKs code against: pin every
// row, including the two that had no test until now (403, 500).
func TestFilesErrorStatusMap(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"missing", fs.ErrNotExist, http.StatusNotFound},
		{"permission denied", fs.ErrPermission, http.StatusForbidden},
		{"destination is a directory", errDestIsDir, http.StatusConflict},
		{"already exists", fs.ErrExist, http.StatusConflict},
		{"parent is a file", syscall.ENOTDIR, http.StatusConflict},
		{"read of a directory", syscall.EISDIR, http.StatusConflict},
		{"directory not empty", syscall.ENOTEMPTY, http.StatusConflict},
		{"symlink chain too long", syscall.ELOOP, http.StatusConflict},
		{"disk full", syscall.ENOSPC, http.StatusInternalServerError},
		{"anything else", errors.New("unexpected"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fsErrorStatus(c.err); got != c.want {
				t.Fatalf("fsErrorStatus(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// A file the executor cannot read answers with the documented 403.
func TestFilesGetForbidden(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; no 403 to observe")
	}
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "locked")
	os.WriteFile(path, []byte("x"), 0o644)
	t.Cleanup(func() { os.Chmod(path, 0o644) })
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	expectError(t, serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", path), nil)), http.StatusForbidden)
}

// A current If-None-Match saves the transfer: 304, no body.
func TestFilesGetNotModified(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("hello"), 0o644)

	head := serve(mux, newAuthRequest(http.MethodHead, filesURL("/files", path), nil))
	etag := head.Header().Get("ETag")
	if etag == "" {
		t.Fatal("HEAD returned no ETag")
	}

	req := newAuthRequest(http.MethodGet, filesURL("/files", path), nil)
	req.Header.Set("If-None-Match", etag)
	if w := serve(mux, req); w.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match current: %d", w.Code)
	} else if w.Body.Len() != 0 {
		t.Errorf("304 carried a body: %q", w.Body.String())
	}

	req = newAuthRequest(http.MethodGet, filesURL("/files", path), nil)
	req.Header.Set("If-None-Match", `"stale"`)
	if w := serve(mux, req); w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("If-None-Match stale: %d %q", w.Code, w.Body.String())
	}
}
