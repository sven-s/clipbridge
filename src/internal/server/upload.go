package server

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// UploadChunkSize is the slice size clients are told to use. Cloudflare caps
	// request bodies at 100 MB on Free/Pro plans, so anything larger than that
	// gets a 413 before it ever reaches us. 64 MiB leaves room for headers and
	// proxy overhead. Tailscale Funnel has no such cap, but one code path beats
	// two — a 64 MiB chunk is not slow enough anywhere to be worth special-casing.
	UploadChunkSize = 64 << 20

	// uploadTTL is how long an incomplete upload's staging data survives. Long
	// enough to outlast a corporate proxy stalling a chunk for ages, short
	// enough that abandoned transfers don't accumulate. Distinct from slots,
	// which are never auto-cleared.
	uploadTTL = 24 * time.Hour
)

// UploadFile is one file within an upload session, declared up front so the
// server can validate sizes at completion instead of trusting the last write.
type UploadFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type uploadSession struct {
	id      string
	dir     string
	files   []UploadFile
	zipName string
	created time.Time
}

// uploadIDLen is in bytes; the hex form is twice this.
const uploadIDLen = 16

func newUploadID() (string, error) {
	b := make([]byte, uploadIDLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validUploadID guards the staging path built from it. Only the exact shape we
// generate is accepted, so no separators or dots can reach filepath.Join.
func validUploadID(id string) bool {
	if len(id) != uploadIDLen*2 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type uploadManager struct {
	mu       sync.Mutex
	sessions map[string]*uploadSession
	stageDir string
}

func newUploadManager(stageDir string) *uploadManager {
	m := &uploadManager{
		sessions: make(map[string]*uploadSession),
		stageDir: stageDir,
	}
	// Anything already on disk is orphaned by definition: sessions live only in
	// memory, so a restart leaves no way to resume them.
	os.RemoveAll(stageDir)
	os.MkdirAll(stageDir, 0700)

	// Sweeping only on init would let an abandoned multi-GB upload hold disk
	// indefinitely on an otherwise idle server.
	go func() {
		for range time.Tick(time.Hour) {
			m.sweep()
		}
	}()
	return m
}

func (m *uploadManager) sessionDir(id string) string {
	return filepath.Join(m.stageDir, id)
}

func (m *uploadManager) get(id string) *uploadSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *uploadManager) put(sess *uploadSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.id] = sess
}

func (m *uploadManager) drop(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	os.RemoveAll(m.sessionDir(id))
}

// sweep discards sessions whose staging data has aged out.
func (m *uploadManager) sweep() {
	m.mu.Lock()
	var stale []string
	for id, sess := range m.sessions {
		if time.Since(sess.created) > uploadTTL {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, id := range stale {
		os.RemoveAll(m.sessionDir(id))
	}
}

// handleUpload dispatches /upload/... by its first path segment. Written by hand
// rather than with per-route patterns so the code keeps working on Go 1.21,
// which has no method-aware mux.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/upload/")
	action, tail, _ := strings.Cut(rest, "/")

	switch action {
	case "init":
		s.handleUploadInit(w, r)
	case "chunk":
		s.handleUploadChunk(w, r, tail)
	case "complete":
		s.handleUploadComplete(w, r, tail)
	case "abort":
		s.handleUploadAbort(w, r, tail)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Dir     string       `json:"dir"`
		Files   []UploadFile `json:"files"`
		ZipName string       `json:"zipName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validDir(body.Dir) {
		http.Error(w, "invalid direction", http.StatusBadRequest)
		return
	}
	if len(body.Files) == 0 {
		http.Error(w, "no files declared", http.StatusBadRequest)
		return
	}
	for _, f := range body.Files {
		if f.Size < 0 {
			http.Error(w, "invalid file size", http.StatusBadRequest)
			return
		}
	}

	id, err := newUploadID()
	if err != nil {
		http.Error(w, "id error", http.StatusInternalServerError)
		return
	}
	sess := &uploadSession{
		id:      id,
		dir:     body.Dir,
		files:   body.Files,
		zipName: body.ZipName,
		created: time.Now(),
	}
	if err := os.MkdirAll(s.uploads.sessionDir(id), 0700); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	s.uploads.put(sess)
	s.uploads.sweep()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"uploadId":  id,
		"chunkSize": UploadChunkSize,
	})
}

// handleUploadChunk writes one slice at its declared offset. Offsets rather than
// sequence numbers mean a retried chunk overwrites itself harmlessly, so a
// client can retry a dropped chunk without restarting the file.
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request, tail string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, idxStr, _ := strings.Cut(tail, "/")
	if !validUploadID(id) {
		http.Error(w, "bad upload id", http.StatusBadRequest)
		return
	}
	sess := s.uploads.get(id)
	if sess == nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(sess.files) {
		http.Error(w, "bad file index", http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 || offset > sess.files[idx].Size {
		http.Error(w, "bad offset", http.StatusBadRequest)
		return
	}

	partPath := filepath.Join(s.uploads.sessionDir(id), strconv.Itoa(idx)+".part")
	f, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		http.Error(w, "seek error", http.StatusInternalServerError)
		return
	}

	// Cap the read so a client cannot grow the file past what it declared.
	limit := sess.files[idx].Size - offset
	buf := make([]byte, 1<<20)
	n, err := io.CopyBuffer(f, io.LimitReader(r.Body, limit), buf)
	if err != nil {
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"received": n, "offset": offset + n})
}

func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validUploadID(id) {
		http.Error(w, "bad upload id", http.StatusBadRequest)
		return
	}
	sess := s.uploads.get(id)
	if sess == nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}

	// Every part must be exactly the size declared at init. This is the only
	// integrity check there is: a chunk silently truncated by a proxy shows up
	// here rather than as a corrupt file on the Mac.
	for i, f := range sess.files {
		partPath := filepath.Join(s.uploads.sessionDir(id), strconv.Itoa(i)+".part")
		fi, err := os.Stat(partPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("missing data for %q", f.Name), http.StatusBadRequest)
			return
		}
		if fi.Size() != f.Size {
			http.Error(w, fmt.Sprintf("size mismatch for %q: got %d, expected %d", f.Name, fi.Size(), f.Size), http.StatusBadRequest)
			return
		}
	}

	slotBase := filepath.Join(s.slotsDir, sess.dir)
	var meta SlotMeta

	if len(sess.files) == 1 {
		// The staging part is already the finished file, and staging lives under
		// the same directory tree as the slots, so this is a rename rather than
		// a second full copy of a potentially multi-GB file.
		partPath := filepath.Join(s.uploads.sessionDir(id), "0.part")
		if err := os.Rename(partPath, slotBase+".data"); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		meta = SlotMeta{Type: "file", Filename: safeName(sess.files[0].Name), Size: sess.files[0].Size}
	} else {
		zipName := sess.zipName
		if zipName == "" {
			zipName = fmt.Sprintf("files-%d.zip", time.Now().Unix())
		}
		if err := s.zipParts(sess, slotBase+".data"); err != nil {
			os.Remove(slotBase + ".data")
			http.Error(w, "zip error", http.StatusInternalServerError)
			return
		}
		fi, err := os.Stat(slotBase + ".data")
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		meta = SlotMeta{Type: "file", Filename: safeName(zipName), Size: fi.Size()}
	}

	metaBytes, _ := json.Marshal(meta)
	if err := os.WriteFile(slotBase+".meta", metaBytes, 0600); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.meta[sess.dir] = &meta
	s.mu.Unlock()

	s.uploads.drop(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUploadAbort(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validUploadID(id) {
		http.Error(w, "bad upload id", http.StatusBadRequest)
		return
	}
	s.uploads.drop(id)
	w.WriteHeader(http.StatusNoContent)
}

// zipParts streams the staged parts into a zip at dest. Nothing is buffered
// whole; each part is copied through a 1 MiB window.
func (s *Server) zipParts(sess *uploadSession, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	buf := make([]byte, 1<<20)
	for i, f := range sess.files {
		partPath := filepath.Join(s.uploads.sessionDir(sess.id), strconv.Itoa(i)+".part")
		in, err := os.Open(partPath)
		if err != nil {
			zw.Close()
			return err
		}
		fw, err := zw.Create(safeName(f.Name))
		if err != nil {
			in.Close()
			zw.Close()
			return err
		}
		if _, err := io.CopyBuffer(fw, in, buf); err != nil {
			in.Close()
			zw.Close()
			return err
		}
		in.Close()
	}
	return zw.Close()
}

// safeName strips any directory component a client may have sent, so a name like
// "../../evil" cannot steer where the file lands or what a zip entry claims to be.
func safeName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "/" {
		return "file"
	}
	return name
}
