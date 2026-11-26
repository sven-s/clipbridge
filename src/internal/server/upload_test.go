package server

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testSecret = "test-secret"

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s := New(testSecret, dir)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, method, url string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testSecret)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// initUpload starts a session and returns its id and the server's chunk size.
func initUpload(t *testing.T, base, dir string, files []UploadFile, zipName string) (string, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"dir": dir, "files": files, "zipName": zipName})
	resp := do(t, http.MethodPost, base+"/upload/init", bytes.NewReader(body), "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("init: status %d", resp.StatusCode)
	}
	var out struct {
		UploadID  string `json:"uploadId"`
		ChunkSize int    `json:"chunkSize"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.UploadID, out.ChunkSize
}

// pushChunks uploads data for one file index, slicing at chunk.
func pushChunks(t *testing.T, base, id string, idx int, data []byte, chunk int) {
	t.Helper()
	if len(data) == 0 {
		resp := do(t, http.MethodPost, fmt.Sprintf("%s/upload/chunk/%s/%d?offset=0", base, id, idx), bytes.NewReader(nil), "application/octet-stream")
		resp.Body.Close()
		return
	}
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		url := fmt.Sprintf("%s/upload/chunk/%s/%d?offset=%d", base, id, idx, off)
		resp := do(t, http.MethodPost, url, bytes.NewReader(data[off:end]), "application/octet-stream")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chunk at %d: status %d", off, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func completeUpload(t *testing.T, base, id string) *http.Response {
	t.Helper()
	return do(t, http.MethodPost, base+"/upload/complete/"+id, nil, "")
}

// TestChunkedUploadRoundTrip pushes a payload several chunks long and checks the
// reassembled slot byte for byte. Chunk size is forced small so the boundary
// logic is exercised without moving 64 MiB per test.
func TestChunkedUploadRoundTrip(t *testing.T) {
	s, ts := newTestServer(t)

	data := make([]byte, 5*1024+7) // deliberately not a chunk multiple
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)

	id, _ := initUpload(t, ts.URL, "from-TESTPC", []UploadFile{{Name: "big.bin", Size: int64(len(data))}}, "")
	pushChunks(t, ts.URL, id, 0, data, 1024)

	resp := completeUpload(t, ts.URL, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("complete: status %d: %s", resp.StatusCode, b)
	}

	meta := s.PeekMeta("from-TESTPC")
	if meta == nil {
		t.Fatal("no meta after complete")
	}
	if meta.Filename != "big.bin" || meta.Size != int64(len(data)) {
		t.Fatalf("meta = %+v, want big.bin/%d", meta, len(data))
	}

	got, err := os.ReadFile(headData(t, s, "from-TESTPC"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != want {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

// TestChunkRetryIsIdempotent resends a chunk, mimicking a client retry after a
// dropped connection. Offsets must make that a no-op, not a duplicate append.
func TestChunkRetryIsIdempotent(t *testing.T) {
	s, ts := newTestServer(t)

	data := []byte("0123456789abcdefghij")
	id, _ := initUpload(t, ts.URL, "from-TESTPC", []UploadFile{{Name: "f.txt", Size: int64(len(data))}}, "")

	// Send every chunk twice, out of order relative to itself.
	pushChunks(t, ts.URL, id, 0, data, 5)
	pushChunks(t, ts.URL, id, 0, data, 5)

	resp := completeUpload(t, ts.URL, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("complete: status %d: %s", resp.StatusCode, b)
	}

	got, err := os.ReadFile(headData(t, s, "from-TESTPC"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

// TestCompleteRejectsShortUpload is the integrity check that matters: a proxy
// truncating a chunk must fail loudly at completion, not land a corrupt file.
func TestCompleteRejectsShortUpload(t *testing.T) {
	s, ts := newTestServer(t)

	id, _ := initUpload(t, ts.URL, "from-TESTPC", []UploadFile{{Name: "f.bin", Size: 100}}, "")
	pushChunks(t, ts.URL, id, 0, make([]byte, 40), 40) // 60 bytes short

	resp := completeUpload(t, ts.URL, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if s.PeekMeta("from-TESTPC") != nil {
		t.Fatal("slot was populated despite a short upload")
	}
}

// TestChunkCannotExceedDeclaredSize stops a client from growing a file past what
// it declared, which is what bounds the disk a session can consume.
func TestChunkCannotExceedDeclaredSize(t *testing.T) {
	s, ts := newTestServer(t)

	id, _ := initUpload(t, ts.URL, "from-TESTPC", []UploadFile{{Name: "f.bin", Size: 10}}, "")
	resp := do(t, http.MethodPost, ts.URL+"/upload/chunk/"+id+"/0?offset=0",
		bytes.NewReader(make([]byte, 5000)), "application/octet-stream")
	resp.Body.Close()

	part := filepath.Join(s.slotsDir, ".uploads", id, "0.part")
	fi, err := os.Stat(part)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 10 {
		t.Fatalf("part size = %d, want 10 (excess must be discarded)", fi.Size())
	}
}

// TestMultiFileUploadZips covers the many-files path: each file is chunked
// separately, and completion produces one zip in the slot.
func TestMultiFileUploadZips(t *testing.T) {
	s, ts := newTestServer(t)

	a := bytes.Repeat([]byte("A"), 3000)
	b := bytes.Repeat([]byte("B"), 1500)
	files := []UploadFile{{Name: "a.txt", Size: int64(len(a))}, {Name: "b.txt", Size: int64(len(b))}}

	id, _ := initUpload(t, ts.URL, "from-TESTPC", files, "bundle.zip")
	pushChunks(t, ts.URL, id, 0, a, 1024)
	pushChunks(t, ts.URL, id, 1, b, 1024)

	resp := completeUpload(t, ts.URL, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("complete: status %d: %s", resp.StatusCode, bodyBytes)
	}

	meta := s.PeekMeta("from-TESTPC")
	if meta == nil || meta.Filename != "bundle.zip" {
		t.Fatalf("meta = %+v, want bundle.zip", meta)
	}

	zr, err := zip.OpenReader(headData(t, s, "from-TESTPC"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != 2 {
		t.Fatalf("zip has %d entries, want 2", len(zr.File))
	}
	for i, want := range [][]byte{a, b} {
		rc, err := zr.File[i].Open()
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("entry %d (%s): %d bytes, want %d", i, zr.File[i].Name, len(got), len(want))
		}
	}
}

// TestZeroByteFile guards the edge where there are no chunks to loop over.
func TestZeroByteFile(t *testing.T) {
	s, ts := newTestServer(t)

	id, _ := initUpload(t, ts.URL, "from-TESTPC", []UploadFile{{Name: "empty.txt", Size: 0}}, "")
	pushChunks(t, ts.URL, id, 0, nil, 1024)

	resp := completeUpload(t, ts.URL, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("complete: status %d: %s", resp.StatusCode, b)
	}
	if meta := s.PeekMeta("from-TESTPC"); meta == nil || meta.Size != 0 {
		t.Fatalf("meta = %+v, want size 0", meta)
	}
}

func TestUploadRejectsBadInput(t *testing.T) {
	_, ts := newTestServer(t)

	t.Run("traversal in dir", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"dir":   "to-../../escape",
			"files": []UploadFile{{Name: "x", Size: 1}},
		})
		resp := do(t, http.MethodPost, ts.URL+"/upload/init", bytes.NewReader(body), "application/json")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("malformed upload id", func(t *testing.T) {
		resp := do(t, http.MethodPost, ts.URL+"/upload/chunk/..%2f..%2fetc/0?offset=0", bytes.NewReader([]byte("x")), "application/octet-stream")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 400 or 404", resp.StatusCode)
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		resp := do(t, http.MethodPost, ts.URL+"/upload/chunk/"+"ab12"+"/0?offset=0", bytes.NewReader([]byte("x")), "application/octet-stream")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("no auth", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/upload/init", "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestSlotHandlersRejectTraversal covers the pre-existing endpoints, which
// accepted "to-../../x" before validDir existed.
func TestSlotHandlersRejectTraversal(t *testing.T) {
	_, ts := newTestServer(t)

	for _, path := range []string{"/poll/to-../../x", "/receive/to-../../x"} {
		resp := do(t, http.MethodGet, ts.URL+path, nil, "")
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s returned 200; traversal not blocked", path)
		}
	}
}

// headData returns the on-disk path of the oldest item in a from- queue.
func headData(t *testing.T, s *Server, dir string) string {
	t.Helper()
	path, meta := s.head(dir)
	if meta == nil {
		t.Fatalf("%s: queue empty", dir)
	}
	return path
}

func uploadOne(t *testing.T, base, dir, name string, data []byte) {
	t.Helper()
	id, _ := initUpload(t, base, dir, []UploadFile{{Name: name, Size: int64(len(data))}}, "")
	pushChunks(t, base, id, 0, data, 1024)
	resp := completeUpload(t, base, id)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("complete %s: status %d", name, resp.StatusCode)
	}
}

// TestFromQueueKeepsEveryItem is the reason the queue exists: consecutive sends
// from a browser must all reach the Mac, oldest first, not just the last one.
func TestFromQueueKeepsEveryItem(t *testing.T) {
	s, ts := newTestServer(t)

	uploadOne(t, ts.URL, "from-TESTPC", "a.txt", []byte("aaa"))
	resp := do(t, http.MethodPost, ts.URL+"/send/from-TESTPC", bytes.NewReader([]byte("hello")), "text/plain")
	resp.Body.Close()
	uploadOne(t, ts.URL, "from-TESTPC", "b.txt", []byte("bbbb"))

	items := s.Pending("from-TESTPC")
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0].Meta.Filename != "a.txt" || items[1].Meta.Type != "text" || items[2].Meta.Filename != "b.txt" {
		t.Fatalf("wrong order: %+v", items)
	}

	dest := t.TempDir()
	p, err := s.CopyItem("from-TESTPC", items[0], dest)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "aaa" {
		t.Fatalf("copied %q", got)
	}
	if txt, _ := s.ReadItemText("from-TESTPC", items[1].ID); txt != "hello" {
		t.Fatalf("text = %q", txt)
	}
	s.RemoveItem("from-TESTPC", items[0].ID)
	if m := s.PeekMeta("from-TESTPC"); m == nil || m.Type != "text" {
		t.Fatalf("head after remove = %+v", m)
	}

	// A restart must see the same queue.
	s2 := New(testSecret, s.slotsDir)
	if got := s2.Pending("from-TESTPC"); len(got) != 2 || got[0].ID != items[1].ID || got[1].ID != items[2].ID {
		t.Fatalf("after restart: %+v", got)
	}
}

// TestToSlotStillOverwrites guards the other direction, which stays single-value.
func TestToSlotStillOverwrites(t *testing.T) {
	s, ts := newTestServer(t)
	for _, body := range []string{"one", "two"} {
		resp := do(t, http.MethodPost, ts.URL+"/send/to-TESTPC", bytes.NewReader([]byte(body)), "text/plain")
		resp.Body.Close()
	}
	got, _ := os.ReadFile(filepath.Join(s.slotsDir, "to-TESTPC.data"))
	if string(got) != "two" {
		t.Fatalf("to- slot = %q, want two", got)
	}
}

// TestLegacyFromSlotMigrates covers upgrading with an item still waiting in the
// pre-queue flat layout.
func TestLegacyFromSlotMigrates(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "from-OLD.data"), []byte("legacy"), 0600)
	os.WriteFile(filepath.Join(dir, "from-OLD.meta"), []byte(`{"type":"file","filename":"x.bin","size":6}`), 0600)

	s := New(testSecret, dir)
	items := s.Pending("from-OLD")
	if len(items) != 1 || items[0].Meta.Filename != "x.bin" {
		t.Fatalf("items = %+v", items)
	}
	if _, err := os.Stat(filepath.Join(dir, "from-OLD.meta")); !os.IsNotExist(err) {
		t.Fatal("legacy meta not removed")
	}
	if got, _ := os.ReadFile(headData(t, s, "from-OLD")); string(got) != "legacy" {
		t.Fatalf("data = %q", got)
	}
}
