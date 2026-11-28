// Package bridge pkg/skychat/bridge/state.go c3-app-chat
package bridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// State is what the bridge keeps across restarts: where each side resumes
// and the lines accepted but not yet delivered.
type State struct {
	// TelegramOffset is the next getUpdates offset.
	TelegramOffset int64 `json:"telegram_offset"`
	// SkychatSinceNs is the newest group message timestamp seen. A restart
	// replays the group from here.
	SkychatSinceNs int64 `json:"skychat_since_ns"`
	// SkychatSeen holds recent "ts:pk" keys so a replayed message is not
	// relayed twice.
	SkychatSeen []string `json:"skychat_seen,omitempty"`

	ToTelegram []QueuedLine `json:"to_telegram,omitempty"`
	ToSkychat  []QueuedLine `json:"to_skychat,omitempty"`
}

// QueuedLine is one message waiting to be delivered. SenderPK is set for
// skychat messages, whose display name is looked up at delivery.
type QueuedLine struct {
	SenderPK string `json:"sender_pk,omitempty"`
	Text     string `json:"text"`
}

// StateLoad reads the state file. A missing file is an empty state.
func StateLoad(path string) (State, error) {
	var s State
	raw, err := os.ReadFile(path) //nolint:gosec // operator supplied path
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(raw, &s)
	return s, err
}

// StateSave writes the state file through a rename, so a crash leaves the
// old or the new file and never half of one.
func StateSave(path string, s State) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
