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
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
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

	// 0777 is the upper bound of the mode parse, and an explicit mode
	// replaces an existing file's mode rather than inheriting it.
	if w := putFile(t, mux, path, []byte("x"), "mode", "0777"); w.Code != http.StatusOK {
		t.Fatalf("mode 0777: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o777 {
		t.Errorf("mode 0777: got %o", info.Mode().Perm())
	}
	if w := putFile(t, mux, path, []byte("y"), "mode", "0600"); w.Code != http.StatusOK {
		t.Fatalf("mode onto existing: expected 200, got %d", w.Code)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode onto existing: got %o", info.Mode().Perm())
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

// A symlink loop never resolves: like /write_file, the PUT must fail rather
// than rename the upload over the final link.
func TestFilesPutSymlinkLoop(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}

	expectError(t, putFile(t, mux, loop, []byte("new")), http.StatusInternalServerError)
	if fi, _ := os.Lstat(loop); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
	if got, _ := os.Readlink(loop); got != loop {
		t.Errorf("link target changed: %q", got)
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
}

// The kernel resolves at most 40 links per path: a chain of exactly 40 must
// write, and the 41st link is where it refuses with ELOOP.
func TestFilesPutSymlinkChainLimits(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("old"), 0o600)

	// l40 → l39 → … → l0 → target.
	p := target
	for i := range 41 {
		link := filepath.Join(dir, fmt.Sprintf("l%d", i))
		if err := os.Symlink(p, link); err != nil {
			t.Fatal(err)
		}
		p = link
	}

	if w := putFile(t, mux, filepath.Join(dir, "l39"), []byte("new")); w.Code != http.StatusOK {
		t.Fatalf("40 links: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("target holds %q", got)
	}

	entry := filepath.Join(dir, "l40")
	w := putFile(t, mux, entry, []byte("x"))
	body := w.Body.String()
	expectError(t, w, http.StatusInternalServerError)
	if !strings.Contains(body, entry) {
		t.Errorf("error names the wrong path: %s", body)
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
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

// A rename onto a directory fails: commitTempFile must report it, not let the
// caller believe the file landed.
func TestCommitTempFileRenameFailure(t *testing.T) {
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if err := os.Mkdir(filepath.Join(dir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := commitTempFile(tmp, filepath.Join(dir, "target"), 0o644); err == nil {
		t.Error("expected the rename onto a directory to fail")
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

	// Only a size of 0 takes the raw-stream path; a one-byte file goes
	// through ServeContent like any other.
	one := filepath.Join(filepath.Dir(path), "one")
	os.WriteFile(one, []byte("x"), 0o644)
	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", one), nil))
	if w.Code != http.StatusOK || w.Body.String() != "x" ||
		w.Header().Get("ETag") == "" || w.Header().Get("Content-Length") != "1" {
		t.Fatalf("one-byte file: %d %q etag=%q length=%q", w.Code, w.Body.String(),
			w.Header().Get("ETag"), w.Header().Get("Content-Length"))
	}
}

// /proc files stat as size 0 yet have content: the size-0 path must
// stream what it reads, not an empty body.
func TestFilesGetPseudoFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc only exists on linux")
	}
	_, mux := newTestServer(t)

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", "/proc/self/status"), nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Name:") {
		t.Fatalf("expected a streamed /proc file, got %d %q", w.Code, w.Body.String())
	}
}

// A FIFO or socket is refused without being opened: open(2) on a FIFO would
// block until a writer appears, wedging the request instead of a 400.
func TestFilesGetNonRegular(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", fifo), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET fifo: expected 400, got %d", w.Code)
	}

	sock, err := filepath.Abs(filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	defer l.Close()
	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files", sock), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET socket: expected 400, got %d", w.Code)
	}

	// Stat reports the type instead of refusing the file.
	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/stat", fifo), nil))
	var entry FileEntry
	if err := json.NewDecoder(w.Body).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || entry.Type != "other" {
		t.Fatalf("stat fifo: expected 200 other, got %d %+v", w.Code, entry)
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
	if err := json.NewDecoder(w.Body).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	if entry.Type != "file" || entry.Size != 5 || entry.Mode != "0640" ||
		entry.MTime == "" || entry.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("unexpected stat: %+v", entry)
	}

	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/stat", dir), nil))
	if err := json.NewDecoder(w.Body).Decode(&entry); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("scan: %v", err)
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

	// A directory the executor cannot read; permission bits don't bind root.
	var forbidden string
	if os.Geteuid() != 0 {
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o000); err != nil {
			t.Fatal(err)
		}
		forbidden = filepath.Join(sub, "f")
	}

	type errCase struct {
		name   string
		method string
		route  string
		path   string
		params []string
		body   []byte
		status int
	}
	tests := []errCase{
		{"GET missing", http.MethodGet, "/files", missing, nil, nil, http.StatusNotFound},
		{"stat missing", http.MethodGet, "/files/stat", missing, nil, nil, http.StatusNotFound},
		{"list missing", http.MethodGet, "/files/list", missing, nil, nil, http.StatusNotFound},
		{"PUT missing parent", http.MethodPut, "/files",
			filepath.Join(missing, "f"), nil, []byte("x"), http.StatusNotFound},
		{"PUT onto a directory", http.MethodPut, "/files",
			dir, nil, []byte("x"), http.StatusConflict},
		{"PUT under a file", http.MethodPut, "/files",
			filepath.Join(file, "f"), nil, []byte("x"), http.StatusConflict},
		{"PUT parents under a file", http.MethodPut, "/files",
			filepath.Join(file, "a", "f"), []string{"parents", "1"}, []byte("x"), http.StatusConflict},
		{"GET no path", http.MethodGet, "/files", "", nil, nil, http.StatusBadRequest},
		{"GET a directory", http.MethodGet, "/files", dir, nil, nil, http.StatusBadRequest},
		{"list a file", http.MethodGet, "/files/list", file, nil, nil, http.StatusBadRequest},
		{"stat bad hash", http.MethodGet, "/files/stat", file, []string{"hash", "md5"}, nil, http.StatusBadRequest},
		{"PUT bad mode", http.MethodPut, "/files", file, []string{"mode", "rwx"}, []byte("x"), http.StatusBadRequest},
		{"PUT mode out of range", http.MethodPut, "/files",
			file, []string{"mode", "4755"}, []byte("x"), http.StatusBadRequest},
	}
	if forbidden != "" {
		tests = append(tests, errCase{"GET forbidden", http.MethodGet, "/files",
			forbidden, nil, nil, http.StatusForbidden})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newAuthRequest(tt.method, filesURL(tt.route, tt.path, tt.params...), tt.body)
			expectError(t, serve(mux, req), tt.status)
		})
	}

	req := newAuthRequest(http.MethodPut, filesURL("/files", file), []byte("x"))
	req.Header.Set("X-Content-SHA256", "not-hex")
	expectError(t, serve(mux, req), http.StatusBadRequest)

	// A valid hex string of the wrong length is rejected at the boundary,
	// before any bytes are written.
	req = newAuthRequest(http.MethodPut, filesURL("/files", file), []byte("x"))
	req.Header.Set("X-Content-SHA256", strings.Repeat("a", 62))
	w := serve(mux, req)
	body := w.Body.String()
	expectError(t, w, http.StatusBadRequest)
	if !strings.Contains(body, "64 hex") {
		t.Errorf("short checksum: %q", body)
	}

	// Without a token the routes are closed like the others.
	req = httptest.NewRequest(http.MethodGet, filesURL("/files", file), nil)
	if w := serve(mux, req); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET: %d", w.Code)
	}
	post := serve(mux, newAuthRequest(http.MethodPost, filesURL("/files", file), nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /files: %d", post.Code)
	}
}
