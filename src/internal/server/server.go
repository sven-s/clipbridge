package server

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SlotMeta struct {
	Type     string `json:"type"` // "text" or "file"
	Filename string `json:"filename,omitempty"`
	Size     int64  `json:"size"`
}

type Server struct {
	secret      string
	slotsDir    string
	mu          sync.RWMutex
	meta        map[string]*SlotMeta
	machines    map[string]time.Time // name → last seen
	machDirty   bool
	machFlushed time.Time
	uploads     *uploadManager
	queues      map[string][]QueueItem // from- dirs only; see queue.go
	lastSeq     int64
}

// machineTTL is how long a browser stays listed after its last request.
//
// It is deliberately far longer than the 30 s heartbeat: browsers throttle
// timers in hidden tabs (Chrome drops to ~1/min, then freezes the tab
// entirely), and an RDP session that is minimised or disconnected stops
// firing them at all. A short window made machines vanish from the menu
// whenever the user looked away from the tab.
const machineTTL = 15 * time.Minute

// HealthBody is what /healthz returns; the Mac app matches on it.
const HealthBody = "clipbridge ok\n"

func New(secret, slotsDir string) *Server {
	s := &Server{
		secret:   secret,
		slotsDir: slotsDir,
		meta:     make(map[string]*SlotMeta),
		queues:   make(map[string][]QueueItem),
		machines: make(map[string]time.Time),
		// Staging sits beside the slots so finishing a single-file upload is a
		// rename, not a copy of the whole payload.
		uploads: newUploadManager(filepath.Join(slotsDir, ".uploads")),
	}
	s.restoreSlots()
	s.restoreMachines()
	return s
}

// validDir rejects anything that is not a plain to-/from- slot name. A prefix
// check alone is not enough: "to-../../x" starts with "to-" and would otherwise
// let an authenticated caller write outside the slots directory.
func validDir(dir string) bool {
	if !strings.HasPrefix(dir, "to-") && !strings.HasPrefix(dir, "from-") {
		return false
	}
	if strings.Contains(dir, "..") || strings.ContainsAny(dir, `/\`) {
		return false
	}
	return dir == filepath.Base(dir)
}

func (s *Server) restoreSlots() {
	entries, err := os.ReadDir(s.slotsDir)
	if err != nil {
		return
	}
	legacy := make(map[string]SlotMeta)
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, incomingPrefix):
			// A request that died mid-write.
			os.Remove(filepath.Join(s.slotsDir, name))
		case e.IsDir():
			if validDir(name) && isQueueDir(name) {
				s.loadQueue(name)
			}
		case strings.HasSuffix(name, ".meta"):
			dir := strings.TrimSuffix(name, ".meta")
			data, err := os.ReadFile(filepath.Join(s.slotsDir, name))
			if err != nil {
				continue
			}
			var m SlotMeta
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if isQueueDir(dir) {
				legacy[dir] = m
			} else {
				s.meta[dir] = &m
			}
		}
	}
	for dir, m := range legacy {
		s.migrateLegacy(dir, m)
	}
}

func (s *Server) machinesPath() string {
	return filepath.Join(s.slotsDir, "machines.json")
}

// restoreMachines reloads the machine list so a restart of the app does not
// blank the menu until every browser happens to heartbeat again.
func (s *Server) restoreMachines() {
	data, err := os.ReadFile(s.machinesPath())
	if err != nil {
		return
	}
	var saved map[string]time.Time
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	cutoff := time.Now().Add(-machineTTL)
	for name, t := range saved {
		if t.After(cutoff) {
			s.machines[name] = t
		}
	}
}

// flushMachines writes the list at most twice a minute. Callers hold s.mu.
func (s *Server) flushMachines() {
	if !s.machDirty || time.Since(s.machFlushed) < 30*time.Second {
		return
	}
	data, err := json.Marshal(s.machines)
	if err != nil {
		return
	}
	if os.WriteFile(s.machinesPath(), data, 0o600) == nil {
		s.machDirty = false
		s.machFlushed = time.Now()
	}
}

// touchMachine marks a machine alive. Every authenticated request counts, not
// just /register — a tab whose timers are throttled may still be polling, and
// an upload that takes ten minutes is proof of life on its own.
func (s *Server) touchMachine(name string) {
	if name == "" {
		return
	}
	s.mu.Lock()
	s.machines[name] = time.Now()
	s.machDirty = true
	s.flushMachines()
	s.mu.Unlock()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/ui" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(uiHTML))
			return
		}
		http.NotFound(w, r)
	})
	// Unauthenticated liveness probe: lets the menu bar app check the public
	// path end to end, and tells a tunnel pointed at the wrong port apart from
	// one pointed at us. Reveals nothing beyond "a Clipbridge lives here".
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, HealthBody)
	})
	mux.HandleFunc("/register", s.auth(s.handleRegister))
	mux.HandleFunc("/machines", s.auth(s.handleMachines))
	mux.HandleFunc("/send/", s.auth(s.handleSend))
	mux.HandleFunc("/upload/", s.auth(s.handleUpload))
	mux.HandleFunc("/poll/", s.auth(s.handlePoll))
	mux.HandleFunc("/receive/", s.authFlex(s.handleReceive))
	mux.HandleFunc("/clear/", s.auth(s.handleClear))
	return mux
}

// authFlex accepts auth via header OR ?auth= query param (for browser downloads).
func (s *Server) authFlex(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("auth")
		}
		if token != s.secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.touchMachine(r.URL.Query().Get("machine"))
		next(w, r)
	}
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != s.secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.touchMachine(r.Header.Get("X-Machine-Name"))
		next(w, r)
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.touchMachine(body.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.GetMachines())
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dir := strings.TrimPrefix(r.URL.Path, "/send/")
	if !validDir(dir) {
		http.Error(w, "invalid direction", http.StatusBadRequest)
		return
	}

	// Everything lands in a scratch file first and is committed at the end, so a
	// failed request never clobbers the slot or leaves a half item in a queue.
	dst, err := s.newIncoming()
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	tmp := dst.Name()
	defer os.Remove(tmp) // no-op once committed
	defer dst.Close()
	ct := r.Header.Get("Content-Type")
	var meta SlotMeta

	if strings.Contains(ct, "multipart/form-data") {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "bad multipart header", http.StatusBadRequest)
			return
		}

		// Client declares file count via header. >1 → server zips them on the fly.
		fileCount, _ := strconv.Atoi(r.Header.Get("X-File-Count"))
		if fileCount <= 1 {
			// Single-file fast path
			part, err := mr.NextPart()
			if err != nil {
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			meta = SlotMeta{Type: "file", Filename: safeName(part.FileName())}
			buf := make([]byte, 1<<20)
			n, err := io.CopyBuffer(dst, part, buf)
			if err != nil {
				http.Error(w, "write error", http.StatusInternalServerError)
				return
			}
			meta.Size = n
		} else {
			// Multi-file: stream each part into a zip writer on the slot file
			zipName := r.Header.Get("X-Zip-Filename")
			if zipName == "" {
				zipName = fmt.Sprintf("files-%d.zip", time.Now().Unix())
			}
			meta = SlotMeta{Type: "file", Filename: safeName(zipName)}
			zw := zip.NewWriter(dst)
			buf := make([]byte, 1<<20)
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					zw.Close()
					http.Error(w, "multipart error", http.StatusBadRequest)
					return
				}
				fw, err := zw.Create(part.FileName())
				if err != nil {
					zw.Close()
					http.Error(w, "zip create error", http.StatusInternalServerError)
					return
				}
				if _, err := io.CopyBuffer(fw, part, buf); err != nil {
					zw.Close()
					http.Error(w, "zip write error", http.StatusInternalServerError)
					return
				}
			}
			if err := zw.Close(); err != nil {
				http.Error(w, "zip write error", http.StatusInternalServerError)
				return
			}
			if fi, err := dst.Stat(); err == nil {
				meta.Size = fi.Size()
			}
		}
	} else {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		meta = SlotMeta{Type: "text", Size: int64(len(body))}
		if _, err := dst.Write(body); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}

	if err := dst.Close(); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if err := s.commit(dir, tmp, meta); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimPrefix(r.URL.Path, "/poll/")
	if !validDir(dir) {
		http.Error(w, "invalid direction", http.StatusBadRequest)
		return
	}
	meta := s.PeekMeta(dir)
	if meta == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meta)
}

func (s *Server) handleReceive(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimPrefix(r.URL.Path, "/receive/")
	if !validDir(dir) {
		http.Error(w, "invalid direction", http.StatusBadRequest)
		return
	}
	dataPath, meta := s.head(dir)
	if meta == nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(dataPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "stat error", http.StatusInternalServerError)
		return
	}
	if meta.Type == "file" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, meta.Filename))
		w.Header().Set("Content-Type", "application/octet-stream")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	// ServeContent handles HTTP Range requests → resumable downloads if connection drops
	http.ServeContent(w, r, meta.Filename, stat.ModTime(), f)
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dir := strings.TrimPrefix(r.URL.Path, "/clear/")
	if !validDir(dir) {
		http.Error(w, "invalid direction", http.StatusBadRequest)
		return
	}
	s.ClearSlot(dir)
	w.WriteHeader(http.StatusNoContent)
}

// --- Direct access methods used by the Mac app ---

// PeekMeta returns the to- slot's meta, or the oldest item's for a from- queue.
func (s *Server) PeekMeta(dir string) *SlotMeta {
	_, m := s.head(dir)
	return m
}

func (s *Server) PutText(dir, text string) error {
	slotBase := filepath.Join(s.slotsDir, dir)
	if err := os.WriteFile(slotBase+".data", []byte(text), 0600); err != nil {
		return err
	}
	meta := &SlotMeta{Type: "text", Size: int64(len(text))}
	b, _ := json.Marshal(meta)
	os.WriteFile(slotBase+".meta", b, 0600)
	s.mu.Lock()
	s.meta[dir] = meta
	s.mu.Unlock()
	return nil
}

// PutZip zips the given files and stores them as the slot's data file under zipName.
func (s *Server) PutZip(dir, zipName string, srcPaths []string) error {
	slotBase := filepath.Join(s.slotsDir, dir)
	out, err := os.Create(slotBase + ".data")
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	for _, p := range srcPaths {
		in, err := os.Open(p)
		if err != nil {
			zw.Close()
			out.Close()
			os.Remove(slotBase + ".data")
			return err
		}
		fw, err := zw.Create(filepath.Base(p))
		if err != nil {
			in.Close()
			zw.Close()
			out.Close()
			os.Remove(slotBase + ".data")
			return err
		}
		if _, err := io.Copy(fw, in); err != nil {
			in.Close()
			zw.Close()
			out.Close()
			os.Remove(slotBase + ".data")
			return err
		}
		in.Close()
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	out.Close()
	fi, err := os.Stat(slotBase + ".data")
	if err != nil {
		return err
	}
	meta := &SlotMeta{Type: "file", Filename: zipName, Size: fi.Size()}
	b, _ := json.Marshal(meta)
	os.WriteFile(slotBase+".meta", b, 0600)
	s.mu.Lock()
	s.meta[dir] = meta
	s.mu.Unlock()
	return nil
}

func (s *Server) PutFile(dir, srcPath string) error {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	slotBase := filepath.Join(s.slotsDir, dir)
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(slotBase + ".data")
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	meta := &SlotMeta{Type: "file", Filename: filepath.Base(srcPath), Size: fi.Size()}
	b, _ := json.Marshal(meta)
	os.WriteFile(slotBase+".meta", b, 0600)
	s.mu.Lock()
	s.meta[dir] = meta
	s.mu.Unlock()
	return nil
}

// ClearSlot empties a to- slot, or drops a from- queue entirely.
func (s *Server) ClearSlot(dir string) {
	if isQueueDir(dir) {
		s.mu.Lock()
		delete(s.queues, dir)
		s.mu.Unlock()
		os.RemoveAll(filepath.Join(s.slotsDir, dir))
		return
	}
	slotBase := filepath.Join(s.slotsDir, dir)
	os.Remove(slotBase + ".data")
	os.Remove(slotBase + ".meta")
	s.mu.Lock()
	delete(s.meta, dir)
	s.mu.Unlock()
}

// GetMachines returns names of machines seen within machineTTL, sorted alphabetically.
func (s *Server) GetMachines() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().Add(-machineTTL)
	var names []string
	for name, t := range s.machines {
		if t.After(cutoff) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
