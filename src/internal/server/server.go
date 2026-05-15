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
	secret   string
	slotsDir string
	mu       sync.RWMutex
	meta     map[string]*SlotMeta
	machines map[string]time.Time // name → last seen
}

func New(secret, slotsDir string) *Server {
	s := &Server{
		secret:   secret,
		slotsDir: slotsDir,
		meta:     make(map[string]*SlotMeta),
		machines: make(map[string]time.Time),
	}
	s.restoreSlots()
	return s
}

func (s *Server) restoreSlots() {
	entries, err := os.ReadDir(s.slotsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".meta") {
			dir := strings.TrimSuffix(e.Name(), ".meta")
			data, err := os.ReadFile(filepath.Join(s.slotsDir, e.Name()))
			if err == nil {
				var m SlotMeta
				if json.Unmarshal(data, &m) == nil {
					s.meta[dir] = &m
				}
			}
		}
	}
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
	mux.HandleFunc("/register", s.auth(s.handleRegister))
	mux.HandleFunc("/machines", s.auth(s.handleMachines))
	mux.HandleFunc("/send/", s.auth(s.handleSend))
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
	s.mu.Lock()
	s.machines[body.Name] = time.Now()
	s.mu.Unlock()
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
	if !strings.HasPrefix(dir, "to-") && !strings.HasPrefix(dir, "from-") {
		http.Error(w, "invalid direction", http.StatusBadRequest)
		return
	}

	slotBase := filepath.Join(s.slotsDir, dir)
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
			meta = SlotMeta{Type: "file", Filename: part.FileName()}
			dst, err := os.Create(slotBase + ".data")
			if err != nil {
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			buf := make([]byte, 1<<20)
			n, err := io.CopyBuffer(dst, part, buf)
			dst.Close()
			if err != nil {
				os.Remove(slotBase + ".data")
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
			meta = SlotMeta{Type: "file", Filename: zipName}
			dst, err := os.Create(slotBase + ".data")
			if err != nil {
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			zw := zip.NewWriter(dst)
			buf := make([]byte, 1<<20)
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					zw.Close()
					dst.Close()
					os.Remove(slotBase + ".data")
					http.Error(w, "multipart error", http.StatusBadRequest)
					return
				}
				fw, err := zw.Create(part.FileName())
				if err != nil {
					zw.Close()
					dst.Close()
					os.Remove(slotBase + ".data")
					http.Error(w, "zip create error", http.StatusInternalServerError)
					return
				}
				if _, err := io.CopyBuffer(fw, part, buf); err != nil {
					zw.Close()
					dst.Close()
					os.Remove(slotBase + ".data")
					http.Error(w, "zip write error", http.StatusInternalServerError)
					return
				}
			}
			zw.Close()
			dst.Close()
			fi, _ := os.Stat(slotBase + ".data")
			if fi != nil {
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
		if err := os.WriteFile(slotBase+".data", body, 0600); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}

	metaBytes, _ := json.Marshal(meta)
	os.WriteFile(slotBase+".meta", metaBytes, 0600)
	s.mu.Lock()
	s.meta[dir] = &meta
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimPrefix(r.URL.Path, "/poll/")
	s.mu.RLock()
	meta := s.meta[dir]
	s.mu.RUnlock()
	if meta == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meta)
}

func (s *Server) handleReceive(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimPrefix(r.URL.Path, "/receive/")
	slotBase := filepath.Join(s.slotsDir, dir)
	metaData, err := os.ReadFile(slotBase + ".meta")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var meta SlotMeta
	json.Unmarshal(metaData, &meta)
	f, err := os.Open(slotBase + ".data")
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
	s.ClearSlot(dir)
	w.WriteHeader(http.StatusNoContent)
}

// --- Direct access methods used by the Mac app ---

func (s *Server) PeekMeta(dir string) *SlotMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta[dir]
}

func (s *Server) ReadText(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.slotsDir, dir+".data"))
	return string(data), err
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

func (s *Server) CopyFile(dir, destDir string) (string, error) {
	meta := s.PeekMeta(dir)
	if meta == nil {
		return "", fmt.Errorf("no item in slot %s", dir)
	}
	src := filepath.Join(s.slotsDir, dir+".data")
	destPath := filepath.Join(destDir, meta.Filename)
	if _, err := os.Stat(destPath); err == nil {
		ext := filepath.Ext(meta.Filename)
		base := meta.Filename[:len(meta.Filename)-len(ext)]
		for i := 1; ; i++ {
			candidate := filepath.Join(destDir, fmt.Sprintf("%s (%d)%s", base, i, ext))
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				destPath = candidate
				break
			}
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return destPath, err
}

func (s *Server) ClearSlot(dir string) {
	slotBase := filepath.Join(s.slotsDir, dir)
	os.Remove(slotBase + ".data")
	os.Remove(slotBase + ".meta")
	s.mu.Lock()
	delete(s.meta, dir)
	s.mu.Unlock()
}

// GetMachines returns names of machines that checked in within the last 2 minutes, sorted alphabetically.
func (s *Server) GetMachines() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().Add(-2 * time.Minute)
	var names []string
	for name, t := range s.machines {
		if t.After(cutoff) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
