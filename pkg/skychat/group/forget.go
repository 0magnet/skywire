// Package group pkg/skychat/group/forget.go c3-app-chat
package group

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrForgetLive is returned when Forget is asked to drop a group this visor
// is still in. Leave or delete it first.
var ErrForgetLive = errors.New("group: Forget: leave or delete the group first")

// Forget drops a group this visor left, deleted, was refused or removed
// from: its record, its join requests and its CXO data on disk. Without
// the record nothing reopens the group at startup, so it stays gone. A
// missing record is not an error.
func (m *Manager) Forget(id string) error {
	r, ok, err := m.store.Get(id)
	if err != nil {
		return fmt.Errorf("group: Forget: get: %w", err)
	}
	if !ok {
		return nil
	}
	if !r.Status.Terminal() {
		return ErrForgetLive
	}
	m.stopLeaving(id)
	m.mu.Lock()
	sess, live := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if live {
		_ = sess.Close() //nolint:errcheck
	}
	if err := m.store.DeleteJoinRequests(id); err != nil {
		return fmt.Errorf("group: Forget: join requests: %w", err)
	}
	if err := m.store.Delete(id); err != nil {
		return fmt.Errorf("group: Forget: delete: %w", err)
	}
	return m.removeGroupData(id)
}

// removeGroupData deletes the group's CXO directory, the one Open gives each
// session under DataDir.
func (m *Manager) removeGroupData(id string) error {
	if m.dataDir == "" || id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(m.dataDir, "group", id)); err != nil {
		return fmt.Errorf("group: Forget: remove data: %w", err)
	}
	return nil
}
