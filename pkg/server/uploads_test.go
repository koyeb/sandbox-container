package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- request helpers -------------------------------------------------------

// postUpload opens a session with the given request body.
func postUpload(mux http.Handler, req map[string]any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(req)
	return serve(mux, newAuthRequest(http.MethodPost, "/files/uploads", body))
}

// openUpload opens a session for path and returns its id, failing the test on
// anything but 200 with the {upload_id, max_part_size} shape.
func openUpload(t *testing.T, mux http.Handler, req map[string]any) string {
	t.Helper()
	w := postUpload(mux, req)
	if w.Code != http.StatusOK {
		t.Fatalf("open upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		UploadID    string `json:"upload_id"`
		MaxPartSize int64  `json:"max_part_size"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.UploadID == "" {
		t.Fatal("empty upload_id")
	}
	if resp.MaxPartSize != maxUploadPartSize {
		t.Errorf("max_part_size = %d, want %d (100 MiB)", resp.MaxPartSize, maxUploadPartSize)
	}
	return resp.UploadID
}

func uploadURL(id, suffix string) string {
	return "/files/uploads/" + id + suffix
}

func partRequest(id string, offset int64, body []byte) *http.Request {
	return newAuthRequest(http.MethodPut, uploadURL(id, "")+"?offset="+fmt.Sprint(offset), body)
}

// putUploadPart streams body at offset, optionally with a part checksum.
func putUploadPart(mux http.Handler, id string, offset int64, body []byte, sum string) *httptest.ResponseRecorder {
	req := partRequest(id, offset, body)
	if sum != "" {
		req.Header.Set("X-Content-SHA256", sum)
	}
	return serve(mux, req)
}

func completeUpload(mux http.Handler, id string) *httptest.ResponseRecorder {
	return serve(mux, newAuthRequest(http.MethodPost, uploadURL(id, "/complete"), nil))
}

func uploadStatus(mux http.Handler, id string) *httptest.ResponseRecorder {
	return serve(mux, newAuthRequest(http.MethodGet, uploadURL(id, ""), nil))
}

func abortUpload(mux http.Handler, id string) *httptest.ResponseRecorder {
	return serve(mux, newAuthRequest(http.MethodDelete, uploadURL(id, ""), nil))
}

// decodeRanges decodes a status or 409-complete body's ranges.
func decodeRanges(t *testing.T, w *httptest.ResponseRecorder) []uploadSpan {
	t.Helper()
	var resp struct {
		Error  string       `json:"error"`
		Size   int64        `json:"size"`
		Ranges []uploadSpan `json:"ranges"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("bad JSON %q: %v", w.Body.String(), err)
	}
	if resp.Ranges == nil {
		t.Fatalf("ranges missing in %q", w.Body.String())
	}
	return resp.Ranges
}

func markerPathOf(srv *Server, id string) string {
	return filepath.Join(srv.uploads.dir, id)
}

// endlessReader yields an infinite stream of one byte value without
// allocating.
type endlessReader struct{ b byte }

func (r endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

// --- tests -----------------------------------------------------------------

// The core design case: parts out of order, in parallel, with a retried part
// overwriting the same bytes, complete to a whole file.
func TestUploadsOutOfOrderParallelRetriedParts(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")

	const part = 64 << 10
	content := make([]byte, 3*part)
	for i := range content {
		content[i] = byte(i * 7 / 3)
	}
	id := openUpload(t, mux, map[string]any{"path": path, "size": len(content)})

	// Wave: all three parts concurrently, out of order.
	var wg sync.WaitGroup
	for _, off := range []int64{2 * part, 0, part} {
		wg.Add(1)
		go func(off int64) {
			defer wg.Done()
			w := putUploadPart(mux, id, off, content[off:off+part], "")
			if w.Code != http.StatusNoContent {
				t.Errorf("part at %d: expected 204, got %d: %s", off, w.Code, w.Body.String())
			}
		}(off)
	}
	wg.Wait()

	// A retried part overwrites the same bytes.
	if w := putUploadPart(mux, id, part, content[part:2*part], ""); w.Code != http.StatusNoContent {
		t.Fatalf("retried part: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Received ranges merge into one span.
	sw := uploadStatus(mux, id)
	if sw.Code != http.StatusOK {
		t.Fatalf("status: expected 200, got %d", sw.Code)
	}
	ranges := decodeRanges(t, sw)
	want := []uploadSpan{{Start: 0, End: int64(len(content))}}
	if len(ranges) != 1 || ranges[0] != want[0] {
		t.Fatalf("ranges = %v, want %v", ranges, want)
	}

	w := completeUpload(mux, id)
	if w.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var done struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.NewDecoder(w.Body).Decode(&done); err != nil {
		t.Fatal(err)
	}
	if done.Size != int64(len(content)) {
		t.Errorf("complete size = %d, want %d", done.Size, len(content))
	}
	if done.SHA256 != "" {
		t.Errorf("unrequested complete hashed anyway: %q", done.SHA256)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, content) {
		t.Fatal("bytes on disk differ from the parts sent")
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
}

// Missing ranges at complete answer 409 with what is missing, and the session
// stays resumable.
func TestUploadsMissingRangesAtComplete(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	const size = 40 << 10
	id := openUpload(t, mux, map[string]any{"path": path, "size": size})

	sw := uploadStatus(mux, id)
	if sw.Code != http.StatusOK {
		t.Fatalf("status: expected 200, got %d", sw.Code)
	}
	if ranges := decodeRanges(t, sw); len(ranges) != 0 {
		t.Fatalf("fresh session ranges = %v, want []", ranges)
	}

	// Two disjoint parts, one overlapping pair merging to a single span.
	putUploadPart(mux, id, 0, make([]byte, 6<<10), "")
	putUploadPart(mux, id, 4<<10, make([]byte, 6<<10), "")
	putUploadPart(mux, id, 20<<10, make([]byte, 10<<10), "")

	ranges := decodeRanges(t, uploadStatus(mux, id))
	want := []uploadSpan{{0, 10 << 10}, {20 << 10, 30 << 10}}
	if len(ranges) != len(want) {
		t.Fatalf("ranges = %v, want %v", ranges, want)
	}
	for i, sp := range want {
		if ranges[i] != sp {
			t.Fatalf("ranges = %v, want %v", ranges, want)
		}
	}

	w := completeUpload(mux, id)
	if w.Code != http.StatusConflict {
		t.Fatalf("complete: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	missing := decodeRanges(t, w)
	wantMissing := []uploadSpan{{10 << 10, 20 << 10}, {30 << 10, size}}
	if len(missing) != len(wantMissing) {
		t.Fatalf("missing = %v, want %v", missing, wantMissing)
	}
	for i, sp := range wantMissing {
		if missing[i] != sp {
			t.Fatalf("missing = %v, want %v", missing, wantMissing)
		}
	}

	// The session survives the refused complete: send what was missing.
	putUploadPart(mux, id, 10<<10, make([]byte, 10<<10), "")
	putUploadPart(mux, id, 30<<10, make([]byte, size-30<<10), "")
	if w := completeUpload(mux, id); w.Code != http.StatusOK {
		t.Fatalf("complete after resume: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if info, err := os.Stat(path); err != nil || info.Size() != size {
		t.Fatalf("destination: %v (size %v)", err, info)
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
}

// A part's X-Content-SHA256 is checked while streaming; a mismatch records
// nothing.
func TestUploadsPartChecksum(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	content := []byte("part one two")
	id := openUpload(t, mux, map[string]any{"path": path, "size": len(content)})

	good := sha256.Sum256(content)
	wrongSum := sha256.Sum256([]byte("something else"))
	if w := putUploadPart(mux, id, 0, content, hex.EncodeToString(wrongSum[:])); w.Code != http.StatusBadRequest {
		t.Fatalf("mismatched part: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if ranges := decodeRanges(t, uploadStatus(mux, id)); len(ranges) != 0 {
		t.Fatalf("mismatched part recorded ranges: %v", ranges)
	}

	// A malformed checksum header is refused the same way and records
	// nothing either.
	if w := putUploadPart(mux, id, 0, content, "not-hex"); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed part checksum: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if ranges := decodeRanges(t, uploadStatus(mux, id)); len(ranges) != 0 {
		t.Fatalf("malformed-checksum part recorded ranges: %v", ranges)
	}

	if w := putUploadPart(mux, id, 0, content, strings.ToUpper(hex.EncodeToString(good[:]))); w.Code != http.StatusNoContent {
		t.Fatalf("matching part: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if w := completeUpload(mux, id); w.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	onDisk, _ := os.ReadFile(path)
	if !bytes.Equal(onDisk, content) {
		t.Fatalf("bytes on disk = %q, want %q", onDisk, content)
	}
}

// 413 above max_part_size, both from the declared Content-Length and from
// streaming more bytes than the limit allows; a part crossing the upload
// size is a 400.
func TestUploadsPartTooLarge(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "f")
	id := openUpload(t, mux, map[string]any{"path": path, "size": maxUploadPartSize + 4096})

	// Declared Content-Length above the cap: refused before reading,
	// and nothing of it is recorded.
	req := newAuthRequest(http.MethodPut, "/files/uploads/"+id+"?offset=0", []byte("x"))
	req.ContentLength = maxUploadPartSize + 1
	if w := serve(mux, req); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}
	if ranges := decodeRanges(t, uploadStatus(mux, id)); len(ranges) != 0 {
		t.Fatalf("refused part recorded ranges: %v", ranges)
	}

	// A part of exactly max_part_size is as large as it may be.
	req = newAuthRequest(http.MethodPut, "/files/uploads/"+id+"?offset=0", []byte("x"))
	req.ContentLength = maxUploadPartSize
	if w := serve(mux, req); w.Code != http.StatusNoContent {
		t.Fatalf("declared max_part_size: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// No Content-Length: the stream itself is capped at max_part_size.
	req = newAuthRequest(http.MethodPut, "/files/uploads/"+id+"?offset=0", nil)
	req.Body = io.NopCloser(io.LimitReader(endlessReader{'a'}, maxUploadPartSize+1))
	req.ContentLength = -1
	if w := serve(mux, req); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}

	// A stream that lands on the last byte the upload can hold stops
	// exactly there: past the end is a 400, not the part cap's 413.
	req = newAuthRequest(http.MethodPut, "/files/uploads/"+id+"?offset=4096", nil)
	req.Body = io.NopCloser(io.LimitReader(endlessReader{'a'}, maxUploadPartSize+1))
	req.ContentLength = -1
	if w := serve(mux, req); w.Code != http.StatusBadRequest {
		t.Fatalf("streamed to the last byte: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if ranges := decodeRanges(t, uploadStatus(mux, id)); len(ranges) != 1 || ranges[0] != (uploadSpan{Start: 0, End: 1}) {
		t.Fatalf("ranges = %v, want only the accepted part", ranges)
	}

	// A part that would write past the upload size is refused even when small,
	// with and without a declared length, and by the length check itself
	// even when fewer bytes actually arrive.
	id2 := openUpload(t, mux, map[string]any{"path": path + "2", "size": 100})
	req = newAuthRequest(http.MethodPut, "/files/uploads/"+id2+"?offset=90", []byte("zz"))
	req.ContentLength = 20
	if w := serve(mux, req); w.Code != http.StatusBadRequest {
		t.Fatalf("short body, declared past size: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if w := putUploadPart(mux, id2, 90, bytes.Repeat([]byte("z"), 20), ""); w.Code != http.StatusBadRequest {
		t.Fatalf("part past size: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	req = newAuthRequest(http.MethodPut, "/files/uploads/"+id2+"?offset=90", nil)
	req.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("z"), 20)))
	req.ContentLength = -1
	if w := serve(mux, req); w.Code != http.StatusBadRequest {
		t.Fatalf("streamed part past size: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if w := putUploadPart(mux, id2, 200, []byte("z"), ""); w.Code != http.StatusBadRequest {
		t.Fatalf("offset past size: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if ranges := decodeRanges(t, uploadStatus(mux, id2)); len(ranges) != 0 {
		t.Fatalf("refused parts recorded ranges: %v", ranges)
	}

	// With nothing recorded, complete reports the whole file missing.
	w := completeUpload(mux, id2)
	if w.Code != http.StatusConflict {
		t.Fatalf("complete with no parts: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if missing := decodeRanges(t, w); len(missing) != 1 || missing[0] != (uploadSpan{Start: 0, End: 100}) {
		t.Fatalf("missing = %v, want the whole file", missing)
	}
}

// The optional whole-file sha256 is checked at complete; a mismatch leaves
// the destination untouched and sweeps the session.
func TestUploadsCompleteChecksum(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	content := []byte("the whole file")
	sum := sha256.Sum256(content)
	wrong := sha256.Sum256([]byte("not the whole file"))

	id := openUpload(t, mux, map[string]any{"path": path, "size": len(content), "sha256": hex.EncodeToString(wrong[:])})
	putUploadPart(mux, id, 0, content, "")
	if w := completeUpload(mux, id); w.Code != http.StatusBadRequest {
		t.Fatalf("checksum mismatch: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("mismatch created the destination: %v", err)
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind after mismatch: %v", tmp)
	}
	if w := uploadStatus(mux, id); w.Code != http.StatusNotFound {
		t.Errorf("session survived a checksum mismatch: %d", w.Code)
	}

	id = openUpload(t, mux, map[string]any{"path": path, "size": len(content), "sha256": strings.ToUpper(hex.EncodeToString(sum[:]))})
	putUploadPart(mux, id, 0, content, "")
	w := completeUpload(mux, id)
	if w.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var done struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.NewDecoder(w.Body).Decode(&done); err != nil {
		t.Fatal(err)
	}
	if done.Size != int64(len(content)) || done.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("complete response = %+v, want size %d sha256 %s", done, len(content), hex.EncodeToString(sum[:]))
	}
}

// Mode at open is applied at complete; without one, an existing
// destination's mode is carried over, and a new file gets 0644.
func TestUploadsModes(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()

	explicit := filepath.Join(dir, "explicit")
	id := openUpload(t, mux, map[string]any{"path": explicit, "size": 1, "mode": "0755"})
	putUploadPart(mux, id, 0, []byte("x"), "")
	if w := completeUpload(mux, id); w.Code != http.StatusOK {
		t.Fatalf("complete: %d: %s", w.Code, w.Body.String())
	}
	if info, _ := os.Stat(explicit); info.Mode().Perm() != 0o755 {
		t.Errorf("explicit mode: got %o, want 0755", info.Mode().Perm())
	}

	carried := filepath.Join(dir, "carried")
	os.WriteFile(carried, []byte("old"), 0o741)
	id = openUpload(t, mux, map[string]any{"path": carried, "size": 1})
	putUploadPart(mux, id, 0, []byte("x"), "")
	if w := completeUpload(mux, id); w.Code != http.StatusOK {
		t.Fatalf("complete: %d: %s", w.Code, w.Body.String())
	}
	if info, _ := os.Stat(carried); info.Mode().Perm() != 0o741 {
		t.Errorf("carried mode: got %o, want 0741", info.Mode().Perm())
	}

	fresh := filepath.Join(dir, "fresh")
	id = openUpload(t, mux, map[string]any{"path": fresh, "size": 1})
	putUploadPart(mux, id, 0, []byte("x"), "")
	if w := completeUpload(mux, id); w.Code != http.StatusOK {
		t.Fatalf("complete: %d: %s", w.Code, w.Body.String())
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o644 {
		t.Errorf("default mode: got %o, want 0644", info.Mode().Perm())
	}
}

// Abort removes the session, its temp file and its marker; everything after
// is a 404.
func TestUploadsAbort(t *testing.T) {
	srv, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	id := openUpload(t, mux, map[string]any{"path": path, "size": 4096})
	putUploadPart(mux, id, 0, []byte("part"), "")

	if w := abortUpload(mux, id); w.Code != http.StatusNoContent {
		t.Fatalf("abort: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
	if _, err := os.Stat(markerPathOf(srv, id)); !os.IsNotExist(err) {
		t.Errorf("marker left behind: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("aborted upload created the destination: %v", err)
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"status":   uploadStatus(mux, id),
		"part":     putUploadPart(mux, id, 0, []byte("x"), ""),
		"complete": completeUpload(mux, id),
		"abort":    abortUpload(mux, id),
	} {
		if w.Code != http.StatusNotFound {
			t.Errorf("%s after abort: expected 404, got %d", name, w.Code)
		}
	}
}

// Open and part validation, and unknown sessions.
func TestUploadsValidation(t *testing.T) {
	_, mux := newTestServer(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		req  map[string]any
		code int
	}{
		{"missing path", map[string]any{"size": 1}, http.StatusBadRequest},
		{"missing size", map[string]any{"path": "/tmp/f"}, http.StatusBadRequest},
		{"negative size", map[string]any{"path": "/tmp/f", "size": -1}, http.StatusBadRequest},
		{"bad mode", map[string]any{"path": "/tmp/f", "size": 1, "mode": "0999"}, http.StatusBadRequest},
		{"bad mode prefix", map[string]any{"path": "/tmp/f", "size": 1, "mode": "0644 "}, http.StatusBadRequest},
		{"bad sha256", map[string]any{"path": "/tmp/f", "size": 1, "sha256": "nothex"}, http.StatusBadRequest},
		{"short sha256", map[string]any{"path": "/tmp/f", "size": 1, "sha256": "a1b2c3"}, http.StatusBadRequest},
		{"destination is a directory", map[string]any{"path": filepath.Join(dir, "d"), "size": 1}, http.StatusConflict},
		{"missing parent", map[string]any{"path": filepath.Join(dir, "no", "f"), "size": 1}, http.StatusNotFound},
	}
	for _, tc := range cases {
		if w := postUpload(mux, tc.req); w.Code != tc.code {
			t.Errorf("%s: expected %d, got %d: %s", tc.name, tc.code, w.Code, w.Body.String())
		}
	}

	if w := serve(mux, newAuthRequest(http.MethodPost, "/files/uploads", []byte("{not json"))); w.Code != http.StatusBadRequest {
		t.Errorf("malformed body: expected 400, got %d", w.Code)
	}

	// Unknown sessions on every route.
	for name, w := range map[string]*httptest.ResponseRecorder{
		"status":   uploadStatus(mux, "nosuchid"),
		"part":     putUploadPart(mux, "nosuchid", 0, []byte("x"), ""),
		"complete": completeUpload(mux, "nosuchid"),
		"abort":    abortUpload(mux, "nosuchid"),
	} {
		if w.Code != http.StatusNotFound {
			t.Errorf("%s unknown id: expected 404, got %d", name, w.Code)
		}
	}

	// Part parameter validation on a live session.
	path := filepath.Join(dir, "f")
	id := openUpload(t, mux, map[string]any{"path": path, "size": 4096})
	for name, w := range map[string]*httptest.ResponseRecorder{
		"bad offset":   serve(mux, newAuthRequest(http.MethodPut, "/files/uploads/"+id+"?offset=x", []byte("x"))),
		"no offset":    serve(mux, newAuthRequest(http.MethodPut, "/files/uploads/"+id, []byte("x"))),
		"neg offset":   serve(mux, newAuthRequest(http.MethodPut, "/files/uploads/"+id+"?offset=-1", []byte("x"))),
		"bad part sum": putUploadPart(mux, id, 0, []byte("x"), "nothex"),
	} {
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, w.Code, w.Body.String())
		}
	}
	if ranges := decodeRanges(t, uploadStatus(mux, id)); len(ranges) != 0 {
		t.Errorf("refused parts recorded ranges: %v", ranges)
	}

	// Unregistered method combinations.
	if w := serve(mux, newAuthRequest(http.MethodPut, "/files/uploads", []byte("x"))); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT collection: expected 405, got %d", w.Code)
	}
}

// Sessions expire after 1h without a part — lazily at the next request and
// via the janitor when abandoned — taking their temp file and marker with
// them.
func TestUploadsExpiry(t *testing.T) {
	srv, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	id := openUpload(t, mux, map[string]any{"path": path, "size": 4096})
	putUploadPart(mux, id, 0, []byte("part"), "")

	ageSession := func(id string) {
		srv.uploads.mu.Lock()
		defer srv.uploads.mu.Unlock()
		srv.uploads.sessions[id].lastPart = time.Now().Add(-uploadSessionTTL - time.Second)
	}

	// The janitor sweeps sessions nobody asks about again, at exactly the
	// TTL rather than a second past it.
	id2 := openUpload(t, mux, map[string]any{"path": path + "2", "size": 4096})
	putUploadPart(mux, id2, 0, []byte("part"), "")
	srv.uploads.mu.Lock()
	since := time.Now()
	srv.uploads.sessions[id2].lastPart = since
	srv.uploads.mu.Unlock()
	srv.uploads.expireSessions(since.Add(uploadSessionTTL))
	if w := uploadStatus(mux, id2); w.Code != http.StatusNotFound {
		t.Fatalf("janitor-expired session: expected 404, got %d", w.Code)
	}
	if _, err := os.Stat(markerPathOf(srv, id2)); !os.IsNotExist(err) {
		t.Errorf("marker left behind: %v", err)
	}

	// A request lazily expires a session the janitor has not reached yet.
	id3 := openUpload(t, mux, map[string]any{"path": path + "3", "size": 4096})
	putUploadPart(mux, id3, 0, []byte("part"), "")
	ageSession(id3)
	if w := uploadStatus(mux, id3); w.Code != http.StatusNotFound {
		t.Fatalf("expired session: expected 404, got %d", w.Code)
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
	for _, id := range []string{id, id3} {
		if _, err := os.Stat(markerPathOf(srv, id)); !os.IsNotExist(err) {
			t.Errorf("marker left behind: %v", err)
		}
	}
}

// A part streaming for longer than the TTL keeps its session alive: every
// chunk that lands counts as activity, so a slow client is never expired
// mid-copy.
func TestUploadsSlowPartKeepsSessionAlive(t *testing.T) {
	srv, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	id := openUpload(t, mux, map[string]any{"path": path, "size": 4096})
	srv.uploads.mu.Lock()
	sess := srv.uploads.sessions[id]
	// The one slow part of this session has been streaming for over a TTL.
	sess.lastPart = time.Now().Add(-uploadSessionTTL - time.Minute)
	since := time.Now()
	srv.uploads.mu.Unlock()

	pw := partWriter{st: srv.uploads, sess: sess, file: sess.file, off: 0, limit: 4096}
	if _, err := pw.Write([]byte("chunk")); err != nil {
		t.Fatal(err)
	}

	// The janitor runs a moment later and must find the session live.
	srv.uploads.expireSessions(since.Add(time.Second))
	if w := uploadStatus(mux, id); w.Code != http.StatusOK {
		t.Fatalf("mid-copy session expired: %d: %s", w.Code, w.Body.String())
	}
}

// A session that cannot be recorded for the startup sweep must not open:
// its temp file would outlive the run with no marker to find it.
func TestUploadsOpenWithoutMarkerFails(t *testing.T) {
	markers := filepath.Join(os.TempDir(), "koyeb-file-uploads")
	os.RemoveAll(markers)
	if err := os.WriteFile(markers, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(markers)

	_, mux := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	if w := postUpload(mux, map[string]any{"path": path, "size": 4096}); w.Code != http.StatusInternalServerError {
		t.Fatalf("marker-less open: expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("marker-less open left temp files: %v", tmp)
	}
}

// Temp files left by a previous executor run are swept at startup through the
// marker registry; sessions do not survive a restart.
func TestUploadsRestartSweepsOrphanedTempFiles(t *testing.T) {
	markers := t.TempDir()
	orphans := t.TempDir()
	tmpPath := filepath.Join(orphans, ".f.koyeb-tmp-abc123")
	if err := os.WriteFile(tmpPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markers, "deadbeef"), []byte(tmpPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	newUploadStore(markers)
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("startup sweep left the orphan temp file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(markers, "deadbeef")); !os.IsNotExist(err) {
		t.Errorf("startup sweep left the marker: %v", err)
	}

	// A fresh executor loses the previous run's live sessions.
	srvA, muxA := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	id := openUpload(t, muxA, map[string]any{"path": path, "size": 4096})
	putUploadPart(muxA, id, 0, []byte("part"), "")

	if _, err := os.Stat(markerPathOf(srvA, id)); err != nil {
		t.Fatalf("marker not created: %v", err)
	}
	_, _ = newTestServer(t) // the restarted executor
	if tmp := listTempFiles(t, dir); len(tmp) != 0 {
		t.Errorf("restart left the previous run's temp file: %v", tmp)
	}
	if _, err := os.Stat(markerPathOf(srvA, id)); !os.IsNotExist(err) {
		t.Errorf("restart left the previous run's marker: %v", err)
	}
}

// A zero-size upload completes without parts.
func TestUploadsZeroSize(t *testing.T) {
	_, mux := newTestServer(t)
	path := filepath.Join(t.TempDir(), "empty")
	id := openUpload(t, mux, map[string]any{"path": path, "size": 0})

	if w := completeUpload(mux, id); w.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("destination: %v", err)
	}
}
