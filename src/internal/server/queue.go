package server

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// QueueItem is one pending payload in a from- queue.
//
// from- slots (browser → Mac) queue instead of overwriting: a user sending
// several files one after another expects all of them to arrive, not just the
// last. to- slots stay single-value — the browser only ever shows one.
//
// On disk: slots/from-NAME/<id>.data + <id>.meta. IDs are zero-padded nanos,
// so lexical order is FIFO order.
type QueueItem struct {
	ID   string
	Meta SlotMeta
}

const incomingPrefix = ".incoming-"

func isQueueDir(dir string) bool { return strings.HasPrefix(dir, "from-") }

func (s *Server) itemBase(dir, id string) string {
	return filepath.Join(s.slotsDir, dir, id)
}

// nextID returns a strictly increasing item ID. Callers hold s.mu.
func (s *Server) nextID() string {
	n := time.Now().UnixNano()
	if n <= s.lastSeq {
		n = s.lastSeq + 1
	}
	s.lastSeq = n
	return fmt.Sprintf("%020d", n)
}

// newIncoming creates a scratch file beside the slots, so commit is a rename.
func (s *Server) newIncoming() (*os.File, error) {
	return os.CreateTemp(s.slotsDir, incomingPrefix+"*")
}

// commit moves a finished payload at src into dir: overwrite for to- slots,
// append for from- queues. src must be on the slots filesystem.
func (s *Server) commit(dir, src string, meta SlotMeta) error {
	metaBytes, _ := json.Marshal(meta)

	if !isQueueDir(dir) {
		base := filepath.Join(s.slotsDir, dir)
		if err := os.Rename(src, base+".data"); err != nil {
			return err
		}
		if err := os.WriteFile(base+".meta", metaBytes, 0600); err != nil {
			return err
		}
		s.mu.Lock()
		s.meta[dir] = &meta
		s.mu.Unlock()
		return nil
	}

	if err := os.MkdirAll(filepath.Join(s.slotsDir, dir), 0700); err != nil {
		return err
	}
	// Held across the renames so concurrent commits append in ID order.
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID()
	base := s.itemBase(dir, id)
	if err := os.Rename(src, base+".data"); err != nil {
		return err
	}
	// .meta last: restore treats a .data without .meta as a torn commit.
	if err := os.WriteFile(base+".meta", metaBytes, 0600); err != nil {
		os.Remove(base + ".data")
		return err
	}
	s.queues[dir] = append(s.queues[dir], QueueItem{ID: id, Meta: meta})
	return nil
}

// loadQueue rebuilds one queue from disk. Callers hold s.mu (or run in New).
func (s *Server) loadQueue(dir string) {
	qdir := filepath.Join(s.slotsDir, dir)
	entries, err := os.ReadDir(qdir) // sorted by name → FIFO
	if err != nil {
		return
	}
	have := make(map[string]bool)
	for _, e := range entries {
		have[e.Name()] = true
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".meta"):
			id := strings.TrimSuffix(name, ".meta")
			if !have[id+".data"] {
				os.Remove(filepath.Join(qdir, name))
				continue
			}
			data, err := os.ReadFile(filepath.Join(qdir, name))
			if err != nil {
				continue
			}
			var m SlotMeta
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			s.queues[dir] = append(s.queues[dir], QueueItem{ID: id, Meta: m})
			if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > s.lastSeq {
				s.lastSeq = n
			}
		case strings.HasSuffix(name, ".data"):
			if !have[strings.TrimSuffix(name, ".data")+".meta"] {
				os.Remove(filepath.Join(qdir, name))
			}
		}
	}
}

// migrateLegacy turns a pre-queue flat from-NAME.data/.meta into a queue item.
func (s *Server) migrateLegacy(dir string, meta SlotMeta) {
	base := filepath.Join(s.slotsDir, dir)
	if err := s.commit(dir, base+".data", meta); err == nil {
		os.Remove(base + ".meta")
	}
}

// head returns the data path and meta of what /receive would serve for dir:
// the slot itself for to-, the oldest item for from-.
func (s *Server) head(dir string) (string, *SlotMeta) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if isQueueDir(dir) {
		q := s.queues[dir]
		if len(q) == 0 {
			return "", nil
		}
		m := q[0].Meta
		return s.itemBase(dir, q[0].ID) + ".data", &m
	}
	m := s.meta[dir]
	if m == nil {
		return "", nil
	}
	c := *m
	return filepath.Join(s.slotsDir, dir+".data"), &c
}

// --- Direct access methods used by the Mac app ---

// Pending returns a snapshot of dir's queue, oldest first.
func (s *Server) Pending(dir string) []QueueItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]QueueItem(nil), s.queues[dir]...)
}

func (s *Server) ReadItemText(dir, id string) (string, error) {
	data, err := os.ReadFile(s.itemBase(dir, id) + ".data")
	return string(data), err
}

// CopyItem copies a queued file into destDir, suffixing " (n)" on name clashes.
func (s *Server) CopyItem(dir string, it QueueItem, destDir string) (string, error) {
	name := safeName(it.Meta.Filename)
	destPath := filepath.Join(destDir, name)
	if _, err := os.Stat(destPath); err == nil {
		ext := filepath.Ext(name)
		stem := name[:len(name)-len(ext)]
		for i := 1; ; i++ {
			candidate := filepath.Join(destDir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				destPath = candidate
				break
			}
		}
	}
	in, err := os.Open(s.itemBase(dir, it.ID) + ".data")
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	if _, err := io.CopyBuffer(out, in, make([]byte, 1<<20)); err != nil {
		out.Close()
		os.Remove(destPath)
		return "", err
	}
	return destPath, out.Close()
}

func (s *Server) RemoveItem(dir, id string) {
	s.mu.Lock()
	q := s.queues[dir]
	for i, it := range q {
		if it.ID == id {
			s.queues[dir] = append(q[:i:i], q[i+1:]...)
			break
		}
	}
	if len(s.queues[dir]) == 0 {
		delete(s.queues, dir)
	}
	s.mu.Unlock()
	base := s.itemBase(dir, id)
	os.Remove(base + ".meta")
	os.Remove(base + ".data")
}
