package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// chTime pins a path's mtime to a known value the tests can assert.
func chTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// readTarGz decodes a gzip'd tar body into its headers and file contents,
// keyed by entry name.
func readTarGz(t *testing.T, body []byte) (map[string]*tar.Header, map[string][]byte) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	headers, content := map[string]*tar.Header{}, map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return headers, content
		}
		if err != nil {
			t.Fatalf("bad tar stream: %v", err)
		}
		headers[hdr.Name] = hdr
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("reading %s: %v", hdr.Name, err)
			}
			content[hdr.Name] = data
		}
	}
}

func TestFilesArchiveGet(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o750)
	top := filepath.Join(dir, "top.txt")
	leaf := filepath.Join(dir, "sub", "leaf")
	topContent := []byte("top-level file")
	leafContent := bytes.Repeat([]byte{0, 1, 2, 253, 254, 255}, 100)
	os.WriteFile(top, topContent, 0o640)
	os.WriteFile(leaf, leafContent, 0o604)
	os.Symlink("sub", filepath.Join(dir, "link"))
	// Special files are skipped, not archived.
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skipf("FIFOs unavailable: %v", err)
	}
	at := time.Date(2024, 5, 4, 3, 2, 1, 0, time.UTC)
	chTime(t, top, at)
	chTime(t, leaf, at)
	chTime(t, filepath.Join(dir, "sub"), at)

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/archive", dir), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type %q, expected application/gzip", ct)
	}

	headers, content := readTarGz(t, w.Body.Bytes())
	// The tree has 4 members; the root itself is not an entry, the symlink
	// is archived as a link, not descended into, and the FIFO is skipped.
	if len(headers) != 4 {
		t.Fatalf("expected 4 entries, got %d: %v", len(headers), headers)
	}
	if _, ok := headers["link/leaf"]; ok {
		t.Error("the walk followed a symlink")
	}

	h := headers["top.txt"]
	if h == nil || h.Typeflag != tar.TypeReg || h.Mode&0o7777 != 0o640 || !h.ModTime.Equal(at) {
		t.Errorf("top.txt header: %+v", h)
	}
	if !bytes.Equal(content["top.txt"], topContent) {
		t.Error("top.txt content differs")
	}
	h = headers["sub"]
	if h == nil || h.Typeflag != tar.TypeDir || h.Mode&0o7777 != 0o750 || !h.ModTime.Equal(at) {
		t.Errorf("sub header: %+v", h)
	}
	h = headers["sub/leaf"]
	if h == nil || h.Typeflag != tar.TypeReg || h.Mode&0o7777 != 0o604 {
		t.Errorf("sub/leaf header: %+v", h)
	}
	if !bytes.Equal(content["sub/leaf"], leafContent) {
		t.Error("sub/leaf content differs")
	}
	h = headers["link"]
	if h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "sub" {
		t.Errorf("link header: %+v", h)
	}

	// A root that is a symlink to a directory archives the directory, like
	// /files/list does.
	w = serve(mux, newAuthRequest(http.MethodGet,
		filesURL("/files/archive", filepath.Join(dir, "link")), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("archive through a symlinked root: %d", w.Code)
	}
	headers, _ = readTarGz(t, w.Body.Bytes())
	if len(headers) != 1 || headers["leaf"] == nil {
		t.Errorf("symlinked root: %+v", headers)
	}
}

func TestFilesArchivePutRoundTrip(t *testing.T) {
	_, mux := newTestServer(t)
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o750)
	top := filepath.Join(src, "top.txt")
	leaf := filepath.Join(src, "sub", "leaf")
	topContent := []byte("round trip")
	leafContent := bytes.Repeat([]byte{7, 8, 250}, 50)
	os.WriteFile(top, topContent, 0o640)
	os.WriteFile(leaf, leafContent, 0o604)
	os.Symlink("sub", filepath.Join(src, "link"))
	at := time.Date(2023, 11, 2, 10, 9, 8, 0, time.UTC)
	chTime(t, top, at)
	chTime(t, leaf, at)
	// Set after the writes, so restoring the directory's mtime is a
	// separate act from creating its contents.
	chTime(t, filepath.Join(src, "sub"), at)

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/archive", src), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	archive := w.Body.Bytes()

	// The destination is created if missing.
	dst := filepath.Join(t.TempDir(), "dst")
	w = serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dst), archive))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d: %q", w.Code, w.Body.String())
	}
	var res struct {
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Bytes != int64(len(topContent)+len(leafContent)) {
		t.Errorf("result %+v, expected 2 files and %d bytes", res, len(topContent)+len(leafContent))
	}

	assertTreeEqual(t, src, dst, false)

	// Extracting into an existing directory merges: a file already there is
	// left alone.
	merged := t.TempDir()
	extra := filepath.Join(merged, "extra.txt")
	os.WriteFile(extra, []byte("keep"), 0o600)
	w = serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", merged), archive))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT into an existing directory: %d", w.Code)
	}
	if got, _ := os.ReadFile(extra); string(got) != "keep" {
		t.Errorf("existing file changed: %q", got)
	}
	assertTreeEqual(t, src, merged, true)
}

// craftTarGz builds a gzip'd tar in memory from entry specs, so tests can
// send archives the executor's own GET would never produce.
func craftTarGz(t *testing.T, entries []tar.Header, content map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := range entries {
		hdr := entries[i]
		data := content[hdr.Name]
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(data))
		} else {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An archive that names anything outside its root is refused whole: ..
// paths, absolute paths, and links whose target leaves the root.
func TestFilesArchivePutTraversal(t *testing.T) {
	_, mux := newTestServer(t)
	parent := t.TempDir()

	cases := []struct {
		name    string
		hdr     tar.Header
		escaped func(dest string) string
	}{
		{
			"dotdot",
			tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
			func(dest string) string { return filepath.Join(filepath.Dir(dest), "escape.txt") },
		},
		{
			"dotdot in a subdirectory",
			tar.Header{Name: "sub/../../escape-nested", Typeflag: tar.TypeReg, Mode: 0o644},
			func(dest string) string { return filepath.Join(filepath.Dir(dest), "escape-nested") },
		},
		{
			"absolute name",
			tar.Header{Name: "/abs-evil", Typeflag: tar.TypeReg, Mode: 0o644},
			func(dest string) string { return filepath.Join(dest, "abs-evil") },
		},
		{
			"symlink escaping the root",
			tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside", Mode: 0o777},
			func(dest string) string { return filepath.Join(dest, "link") },
		},
		{
			"absolute symlink target",
			tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp", Mode: 0o777},
			func(dest string) string { return filepath.Join(dest, "link") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := filepath.Join(parent, "d-"+c.name)
			body := craftTarGz(t, []tar.Header{c.hdr}, map[string][]byte{c.hdr.Name: []byte("evil")})
			w := serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dest), body))
			expectError(t, w, http.StatusBadRequest)
			if _, err := os.Stat(c.escaped(dest)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s exists after the refusal", c.escaped(dest))
			}
		})
	}
}

// The shape tar czf x.tgz -C dir . produces: "./"-prefixed entries whose
// cleaned form is the root itself.
func TestFilesArchivePutDotRootShape(t *testing.T) {
	_, mux := newTestServer(t)
	dest := t.TempDir()
	body := craftTarGz(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./sub/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./sub/f", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "./top.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string][]byte{"./sub/f": []byte("nested"), "./top.txt": []byte("top")})

	w := serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dest), body))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
	}
	var res struct {
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	}
	json.NewDecoder(w.Body).Decode(&res)
	if res.Files != 2 || res.Bytes != 9 {
		t.Errorf("result %+v, expected 2 files and 9 bytes", res)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "top.txt")); string(got) != "top" {
		t.Errorf("top.txt: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "sub", "f")); string(got) != "nested" {
		t.Errorf("sub/f: %q", got)
	}
}

// A hardlink entry is extracted as a hardlink to the member it names, and a
// link whose target leaves the root is refused like a symlink's.
func TestFilesArchivePutHardlink(t *testing.T) {
	_, mux := newTestServer(t)
	parent := t.TempDir()
	dest := filepath.Join(parent, "d")
	body := craftTarGz(t, []tar.Header{
		{Name: "orig", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "orig", Mode: 0o644},
	}, map[string][]byte{"orig": []byte("shared")})

	w := serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dest), body))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
	}
	var res struct {
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	// The hardlink counts as a file of its own, with no content bytes.
	if res.Files != 2 || res.Bytes != int64(len("shared")) {
		t.Errorf("result %+v, expected 2 files and 6 bytes", res)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "hard")); string(got) != "shared" {
		t.Errorf("hard link content: %q", got)
	}
	fi1, _ := os.Stat(filepath.Join(dest, "orig"))
	fi2, _ := os.Stat(filepath.Join(dest, "hard"))
	if !os.SameFile(fi1, fi2) {
		t.Error("hard is not the same file as orig")
	}

	escaping := craftTarGz(t, []tar.Header{
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../outside", Mode: 0o644},
	}, nil)
	w = serve(mux, newAuthRequest(http.MethodPut,
		filesURL("/files/archive", filepath.Join(parent, "d2")), escaping))
	expectError(t, w, http.StatusBadRequest)
	if _, err := os.Lstat(filepath.Join(parent, "d2", "hard")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the escaping hardlink was created")
	}
}

// A PAX global header carries metadata, not a member: the archive around it
// still extracts.
func TestFilesArchivePutGlobalHeader(t *testing.T) {
	_, mux := newTestServer(t)
	dest := t.TempDir()
	body := craftTarGz(t, []tar.Header{
		{Name: "meta", Typeflag: tar.TypeXGlobalHeader},
		{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string][]byte{"f": []byte("after the global header")})

	w := serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dest), body))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
	}
	var res struct {
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	// The global header is not a member: one file, its bytes only.
	if res.Files != 1 || res.Bytes != int64(len("after the global header")) {
		t.Errorf("result %+v, expected 1 file and 22 bytes", res)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "f")); string(got) != "after the global header" {
		t.Errorf("f: %q", got)
	}
}

// An entry with no mtime extracts without one, and a special file is
// refused, not created.
func TestFilesArchivePutSpecialAndZeroTime(t *testing.T) {
	_, mux := newTestServer(t)
	dest := t.TempDir()
	zero := craftTarGz(t,
		[]tar.Header{{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644}},
		map[string][]byte{"f": []byte("x")})
	if w := serve(mux, newAuthRequest(http.MethodPut,
		filesURL("/files/archive", filepath.Join(dest, "d1")), zero)); w.Code != http.StatusOK {
		t.Fatalf("zero mtime: %d: %q", w.Code, w.Body.String())
	}
	// The tar format can't say "no mtime": an entry written with none
	// extracts as the epoch, as tar itself would set it.
	info, err := os.Stat(filepath.Join(dest, "d1", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(time.Unix(0, 0)) {
		t.Errorf("zero mtime extracted as %v, want the epoch", info.ModTime())
	}

	fifo := craftTarGz(t, []tar.Header{{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0o644}}, nil)
	w := serve(mux, newAuthRequest(http.MethodPut,
		filesURL("/files/archive", filepath.Join(dest, "d2")), fifo))
	expectError(t, w, http.StatusBadRequest)
	if _, err := os.Lstat(filepath.Join(dest, "d2", "pipe")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the FIFO was created")
	}
}

func TestFilesArchiveErrors(t *testing.T) {
	_, mux := newTestServer(t)
	root := t.TempDir()
	file := filepath.Join(root, "file")
	os.WriteFile(file, []byte("x"), 0o644)
	missing := filepath.Join(root, "missing")

	get := func(route, path string, params ...string) *httptest.ResponseRecorder {
		return serve(mux, newAuthRequest(http.MethodGet, filesURL(route, path, params...), nil))
	}
	put := func(path string, body []byte) *httptest.ResponseRecorder {
		return serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", path), body))
	}
	valid := craftTarGz(t,
		[]tar.Header{{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644}},
		map[string][]byte{"f": []byte("x")})
	garbage := gzipBytes(t, []byte("definitely not a tar archive"))

	cases := []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
	}{
		{"GET missing root", get("/files/archive", missing), http.StatusNotFound},
		{"GET no path", serve(mux, newAuthRequest(http.MethodGet, "/files/archive", nil)),
			http.StatusBadRequest},
		{"GET a file", get("/files/archive", file), http.StatusBadRequest},
		{"GET bad exclude", get("/files/archive", root, "exclude", "["), http.StatusBadRequest},
		{"PUT no path", serve(mux, newAuthRequest(http.MethodPut, "/files/archive", nil)),
			http.StatusBadRequest},
		{"PUT onto a file", put(file, valid), http.StatusConflict},
		{"PUT body is not gzip", put(filepath.Join(root, "d1"), []byte("not gzip at all")),
			http.StatusBadRequest},
		{"PUT gzip of garbage", put(filepath.Join(root, "d2"), garbage), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Snapshot before expectError: its decoder drains the body.
			body := append([]byte(nil), c.w.Body.Bytes()...)
			expectError(t, c.w, c.status)
			// Every error body is whole-body JSON, as the /files contract
			// documents: nothing may follow it.
			if !json.Valid(body) {
				t.Errorf("error body is not pure JSON: %x", body)
			}
		})
	}

	// A client that goes away mid-stream leaves what it already extracted,
	// and the request fails as a bad body.
	truncated := craftTarGz(t, []tar.Header{
		{Name: "first", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "second", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string][]byte{"first": []byte("one"), "second": []byte("two")})
	cut := truncated[:len(truncated)-20]
	expectError(t, put(filepath.Join(root, "d3"), cut), http.StatusBadRequest)
	if got, _ := os.ReadFile(filepath.Join(root, "d3", "first")); string(got) != "one" {
		t.Errorf("entries extracted before the cut: %q", got)
	}

	// A symlink entry over an existing symlink is a conflict, not a replace.
	dest := filepath.Join(root, "d4")
	os.MkdirAll(dest, 0o755)
	os.Symlink("elsewhere", filepath.Join(dest, "link"))
	overlink := craftTarGz(t, []tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "target", Mode: 0o777},
	}, nil)
	expectError(t, put(dest, overlink), http.StatusConflict)

	// The routes are closed without a token, like the others.
	if w := serve(mux, httptest.NewRequest(http.MethodGet,
		filesURL("/files/archive", root), nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET: %d", w.Code)
	}
	req := newAuthRequest(http.MethodPost, filesURL("/files/archive", root), nil)
	if w := serve(mux, req); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /files/archive: %d", w.Code)
	}
}

// gzipBytes wraps data in its own gzip member, standalone.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Extraction streams: an entry is on disk while the request is still open,
// and the handler is still reading the body — the archive is never held whole
// anywhere.
func TestFilesArchivePutExtractsIncrementally(t *testing.T) {
	_, mux := newTestServer(t)
	dest := t.TempDir()

	// The raw tar of two entries, split after the first one: header (512) +
	// "one" padded to the block size.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range []struct{ name, content string }{
		{"first.txt", "one"},
		{"second.txt", "two"},
	} {
		hdr := &tar.Header{
			Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	all := raw.Bytes()
	first := gzipBytes(t, all[:1024])
	rest := gzipBytes(t, all[1024:])

	pr, pw := io.Pipe()
	req := httptest.NewRequest(http.MethodPut, filesURL("/files/archive", dest), pr)
	req.Header.Set("Authorization", "Bearer test-secret")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(mux, req) }()

	writeErr := make(chan error, 1)
	go func() { _, err := pw.Write(first); writeErr <- err }()
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	// The gzip reader buffers ahead, so poll until the handler has processed
	// the first entry. It can't finish: the pipe is still open, with the
	// second half of the archive unwritten.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dest, "first.txt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first.txt is not on disk while the stream is still open")
		}
		select {
		case w := <-done:
			t.Fatalf("handler finished before the body ended: %d %q", w.Code, w.Body.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case w := <-done:
		t.Fatalf("handler finished before the body ended: %d %q", w.Code, w.Body.String())
	default:
	}

	if _, err := pw.Write(rest); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
		}
		var res struct {
			Files int   `json:"files"`
			Bytes int64 `json:"bytes"`
		}
		if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
			t.Fatal(err)
		}
		if res.Files != 2 || res.Bytes != 6 {
			t.Errorf("result %+v, expected 2 files and 6 bytes", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish after the body ended")
	}
}

// A large incompressible file round trips: bigger than any buffer the
// handlers hold.
func TestFilesArchiveBigRoundTrip(t *testing.T) {
	_, mux := newTestServer(t)
	src := t.TempDir()
	const size = 32 << 20
	rng := rand.New(rand.NewSource(1))
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(rng.Int63())
	}
	os.WriteFile(filepath.Join(src, "big.bin"), content, 0o644)

	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/archive", src), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	w = serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dst), w.Body.Bytes()))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d: %q", w.Code, w.Body.String())
	}
	var res struct {
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Bytes != size {
		t.Errorf("result %+v, expected 1 file and %d bytes", res, size)
	}
	got, err := os.ReadFile(filepath.Join(dst, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("big.bin content differs after the round trip")
	}
}

// A hardlink's Linkname is the archive-root member path of the file it
// links to, not a path relative to the link entry's directory — that is how
// tar itself stores and extracts it.
func TestFilesArchivePutCrossDirHardlink(t *testing.T) {
	_, mux := newTestServer(t)
	dest := t.TempDir()
	body := craftTarGz(t, []tar.Header{
		{Name: "dir1/orig", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "dir2/hard", Typeflag: tar.TypeLink, Linkname: "dir1/orig", Mode: 0o644},
	}, map[string][]byte{"dir1/orig": []byte("shared")})

	w := serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dest), body))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(dest, "dir2", "hard"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "shared" {
		t.Errorf("cross-directory hardlink content: %q", got)
	}
	fi1, _ := os.Stat(filepath.Join(dest, "dir1", "orig"))
	fi2, _ := os.Stat(filepath.Join(dest, "dir2", "hard"))
	if !os.SameFile(fi1, fi2) {
		t.Error("dir2/hard is not the same file as dir1/orig")
	}
}

// A walk that fails before the first entry still writes a clean JSON error
// (no gzip or tar trailer after it), a walk that fails mid-stream ends in a
// cut the client's decompressor fails on, and an empty directory archives
// to a valid empty tar.
func TestFilesArchiveGetWalkError(t *testing.T) {
	_, mux := newTestServer(t)

	// Root the walk cannot read: an error status with a whole-body JSON error.
	locked := t.TempDir()
	os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o644)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	w := serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/archive", locked), nil))
	// Snapshot before expectError: its decoder drains the recorder's body.
	body := append([]byte(nil), w.Body.Bytes()...)
	expectError(t, w, http.StatusForbidden)
	if !json.Valid(body) {
		t.Errorf("error body is not pure JSON: %x", body)
	}

	// Where a walk failure lands depends on what was already streamed:
	// nothing emitted (alone, excluded, skipped) gets a clean error status;
	// committed entries (a directory, a file) get a 200 cut that no client
	// can mistake for a complete archive — no footer, no trailer, no error
	// JSON after the committed status.
	mkfifo := func(t *testing.T, root string) {
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
			t.Skipf("FIFOs unavailable: %v", err)
		}
	}
	for _, tc := range []struct {
		name     string
		excludes []string
		prep     func(t *testing.T, root string)
		wantCut  bool
	}{
		{"unreadable alone", nil, nil, false},
		{"unreadable after exclusions", []string{"skip", "drop.log"}, func(t *testing.T, root string) {
			os.MkdirAll(filepath.Join(root, "skip"), 0o755)
			os.WriteFile(filepath.Join(root, "drop.log"), []byte("x"), 0o644)
		}, false},
		{"unreadable after a skipped special", nil, mkfifo, false},
		{"unreadable after a directory", nil, func(t *testing.T, root string) {
			os.MkdirAll(filepath.Join(root, "sub"), 0o755)
		}, true},
		{"unreadable after a file", nil, func(t *testing.T, root string) {
			os.WriteFile(filepath.Join(root, "a.txt"), []byte("good"), 0o644)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.prep != nil {
				tc.prep(t, root)
			}
			os.WriteFile(filepath.Join(root, "z.txt"), []byte("x"), 0o644)
			if err := os.Chmod(filepath.Join(root, "z.txt"), 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(filepath.Join(root, "z.txt"), 0o644) })
			q := url.Values{"path": {root}}
			for _, ex := range tc.excludes {
				q.Add("exclude", ex)
			}
			w := serve(mux, newAuthRequest(http.MethodGet, "/files/archive?"+q.Encode(), nil))
			if tc.wantCut {
				// Snapshot first: the gzip reader below consumes the body.
				body := append([]byte(nil), w.Body.Bytes()...)
				if w.Code != http.StatusOK {
					t.Fatalf("expected a committed 200 cut, got %d: %q", w.Code, w.Body.String())
				}
				gz, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatalf("not gzip: %v", err)
				}
				if _, err := io.ReadAll(gz); err == nil {
					t.Error("a mid-stream cut decompressed cleanly; the failure is silent")
				}
				if bytes.Contains(body, []byte(`"error"`)) {
					t.Error("the cut stream carries an error body after the committed 200")
				}
				return
			}
			body := append([]byte(nil), w.Body.Bytes()...)
			expectError(t, w, http.StatusForbidden)
			if !json.Valid(body) {
				t.Errorf("error body is not pure JSON: %x", body)
			}
		})
	}

	// An empty directory archives to a valid empty tar.
	w = serve(mux, newAuthRequest(http.MethodGet, filesURL("/files/archive", t.TempDir()), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("empty dir: %d", w.Code)
	}
	headers, _ := readTarGz(t, w.Body.Bytes())
	if len(headers) != 0 {
		t.Errorf("empty dir archived %d entries", len(headers))
	}
}

// A member whose parent directories have no entries of their own extracts
// too, as tar does: directory members are a courtesy, not a requirement.
func TestFilesArchivePutMissingParentEntries(t *testing.T) {
	_, mux := newTestServer(t)
	dest := t.TempDir()
	body := craftTarGz(t, []tar.Header{
		{Name: "a/b/f", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string][]byte{"a/b/f": []byte("orphan member")})

	w := serve(mux, newAuthRequest(http.MethodPut, filesURL("/files/archive", dest), body))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(dest, "a", "b", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "orphan member" {
		t.Errorf("content: %q", got)
	}
}

// goneWriter accepts writes up to limit bytes and then fails, like a client
// that went away mid-download. It records whether a status was set after the
// stream had already committed — the superfluous-WriteHeader case the net/http
// server would log.
type goneWriter struct {
	limit    int
	written  int
	body     bytes.Buffer
	hdr      http.Header
	status   int
	statused bool
	cut      bool
}

func (w *goneWriter) Header() http.Header { return w.hdr }

func (w *goneWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.limit {
		w.cut = true
		return 0, errors.New("client went away")
	}
	w.written += len(p)
	return w.body.Write(p)
}

func (w *goneWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	// A status after content began is the superfluous call net/http logs.
	if w.written > 0 {
		w.statused = true
	}
}

// A copy failure after the first member's header is committed — the client
// hanging up mid-download — is a cut, not an error status: the stream can't
// be answered with one anymore, and nothing may append an error body to it.
func TestFilesArchiveGetWriteFailure(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	content := make([]byte, 1<<20)
	rng := rand.New(rand.NewSource(2))
	for i := range content {
		content[i] = byte(rng.Int63())
	}
	os.WriteFile(filepath.Join(dir, "big.bin"), content, 0o644)

	w := &goneWriter{hdr: http.Header{}, limit: 64 << 10}
	mux.ServeHTTP(w, newAuthRequest(http.MethodGet, filesURL("/files/archive", dir), nil))

	if !w.cut {
		t.Fatal("the cut never happened")
	}
	if w.statused {
		t.Error("a status was set after the stream had committed")
	}
	if bytes.Contains(w.body.Bytes(), []byte(`"error"`)) {
		t.Error("an error body was appended to the committed stream")
	}

	// The mirror image: a write that fails before any byte is delivered
	// (the gzip header itself) still gets a real error status.
	gone := &goneWriter{hdr: http.Header{}, limit: 9}
	mux.ServeHTTP(gone, newAuthRequest(http.MethodGet, filesURL("/files/archive", dir), nil))
	if !gone.cut {
		t.Fatal("the early write did not fail")
	}
	if gone.status != http.StatusInternalServerError {
		t.Errorf("status %d, expected a clean 500 for an uncommitted stream", gone.status)
	}
	if gone.statused {
		t.Error("a status was superfluous")
	}
}

// assertTreeEqual compares the want tree against the got tree: same members
// (unless subset, then want's members must be there), same file contents,
// same modes, same mtimes, same link targets. The roots themselves are not
// compared: they are not archive entries.
func assertTreeEqual(t *testing.T, want, got string, subset bool) {
	t.Helper()
	walk := func(root string) map[string]fs.FileInfo {
		t.Helper()
		got := map[string]fs.FileInfo{}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			got[filepath.ToSlash(rel)] = info
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	wantTree, gotTree := walk(want), walk(got)
	if !subset && len(wantTree) != len(gotTree) {
		t.Fatalf("got %d members, want %d: %v", len(gotTree), len(wantTree), gotTree)
	}
	for rel, wantInfo := range wantTree {
		gotInfo, ok := gotTree[rel]
		if !ok {
			t.Fatalf("missing %q", rel)
		}
		gp := filepath.Join(got, filepath.FromSlash(rel))
		wp := filepath.Join(want, filepath.FromSlash(rel))
		if wantInfo.Mode() != gotInfo.Mode() {
			t.Errorf("%s: mode %o, want %o", rel, gotInfo.Mode(), wantInfo.Mode())
		}
		if wantInfo.Mode()&fs.ModeSymlink != 0 {
			gt, err := os.Readlink(gp)
			if err != nil {
				t.Fatal(err)
			}
			wt, err := os.Readlink(wp)
			if err != nil {
				t.Fatal(err)
			}
			if gt != wt {
				t.Errorf("%s: link target %q, want %q", rel, gt, wt)
			}
			// An extracted symlink's mtime can't be restored (no lchtimes in
			// the standard library); everything else must match.
			continue
		}
		if !gotInfo.ModTime().Equal(wantInfo.ModTime()) {
			t.Errorf("%s: mtime %v, want %v", rel, gotInfo.ModTime(), wantInfo.ModTime())
		}
		if wantInfo.Mode().IsRegular() {
			gc, err := os.ReadFile(gp)
			if err != nil {
				t.Fatal(err)
			}
			wc, err := os.ReadFile(wp)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gc, wc) {
				t.Errorf("%s: content differs", rel)
			}
		}
	}
}

func TestFilesArchiveExclude(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "deep", "node_modules"), 0o755)
	for _, p := range []string{
		"keep.txt",
		"drop.log",
		"deep/keep.go",
		"deep/drop.log",
		"deep/node_modules/pkg.json",
		"deep/node_modules/x.log",
	} {
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	q := url.Values{"path": {dir}, "exclude": {"*.log", "node_modules"}}
	w := serve(mux, newAuthRequest(http.MethodGet, "/files/archive?"+q.Encode(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %q", w.Code, w.Body.String())
	}
	headers, _ := readTarGz(t, w.Body.Bytes())

	// "*.log" drops .log files at any depth; "node_modules" drops the
	// directory at any depth, whole.
	want := map[string]bool{"keep.txt": true, "deep": true, "deep/keep.go": true}
	for name := range headers {
		if !want[name] {
			t.Errorf("unexpected entry %q", name)
		}
	}
	for name := range want {
		if headers[name] == nil {
			t.Errorf("missing entry %q", name)
		}
	}
}
