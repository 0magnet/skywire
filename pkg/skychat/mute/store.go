// Package mute pkg/skychat/mute/store.go c3-app-chat
//
// Package mute keeps which conversations may raise a notification: a switch
// for all of them and a list of muted ones, each named by its thread key (a
// peer's public key hex or a group ID). Local, never sent to a peer.
package mute

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// MaxEntries bounds the muted list, since the file is rewritten whole on a change.
const MaxEntries = 5000

// maxKeyLen bounds a thread key. Keys are 66 hex characters or a group ID.
const maxKeyLen = 128

// ErrFull is returned when the muted list would grow past MaxEntries.
var ErrFull = errors.New("skychat mute: list is full")

// ErrBadKey is returned for an empty or oversized thread key.
var ErrBadKey = errors.New("skychat mute: bad thread key")

type onDisk struct {
	Off   bool     `json:"off"`
	Muted []string `json:"muted"`
}

// Store is the on-disk notification state. Safe for concurrent use.
type Store struct {
	mu    sync.RWMutex
	path  string
	off   bool
	muted map[string]bool
}

// OpenStore opens the store at path. A missing file has notifications on and
// nothing muted.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("skychat mute: OpenStore: path required")
	}
	s := &Store{path: path, muted: map[string]bool{}}
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured app path
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("skychat mute: read %s: %w", path, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return s, nil
	}
	var d onDisk
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("skychat mute: parse %s: %w", path, err)
	}
	s.off = d.Off
	for _, k := range d.Muted {
		if validKey(k) {
			s.muted[k] = true
		}
	}
	return s, nil
}

func validKey(k string) bool { return k != "" && len(k) <= maxKeyLen && strings.TrimSpace(k) == k }

// Enabled is whether notifications are on at all. A nil store has them on.
func (s *Store) Enabled() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.off
}

// Muted is whether the conversation key is muted.
func (s *Store) Muted(key string) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.muted[key]
}

// Allows is whether a message in the conversation key may notify.
func (s *Store) Allows(key string) bool { return s.Enabled() && !s.Muted(key) }

// List is the muted keys, sorted.
func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.muted))
	for k := range s.muted {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetEnabled turns all notifications on or off.
func (s *Store) SetEnabled(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.off = !on
	return s.flushLocked()
}

// SetMuted mutes or unmutes the conversation key.
func (s *Store) SetMuted(key string, muted bool) error {
	if !validKey(key) {
		return ErrBadKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if muted {
		if !s.muted[key] && len(s.muted) >= MaxEntries {
			return ErrFull
		}
		s.muted[key] = true
	} else {
		delete(s.muted, key)
	}
	return s.flushLocked()
}

// flushLocked writes the store through a temp file and a rename, so a crash
// mid-write cannot leave a file OpenStore refuses. Caller holds the lock.
func (s *Store) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("skychat mute: create dir: %w", err)
	}
	d := onDisk{Off: s.off, Muted: make([]string, 0, len(s.muted))}
	for k := range s.muted {
		d.Muted = append(d.Muted, k)
	}
	sort.Strings(d.Muted)
	blob, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("skychat mute: encode: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("skychat mute: write: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("skychat mute: replace: %w", err)
	}
	return nil
}
