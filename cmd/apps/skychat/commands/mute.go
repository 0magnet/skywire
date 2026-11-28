// Package commands cmd/apps/skychat/commands/mute.go c4-app-chat
//
// Notification settings kept on the visor: notifications on or off, and the
// muted conversations. The host-OS path (osnotify.go) honors them, so a mute
// set from the hypervisor UI survives a reload and holds while no browser
// that can notify is attached.
package commands

import (
	"encoding/json"
	"net/http"

	"github.com/skycoin/skywire/pkg/skychat/mute"
)

var muteStore *mute.Store

func muteStorePath() string {
	return identityStatePath(stateDir(), identityScope(), "skychat-mute.json")
}

func openMuteStore(path string) {
	store, err := mute.OpenStore(path)
	if err != nil {
		appLog("Mute: notification settings unavailable (%v), every chat notifies", err)
		return
	}
	muteStore = store
}

type muteState struct {
	Enabled bool     `json:"enabled"`
	Muted   []string `json:"muted"`
}

func currentMute() muteState {
	return muteState{Enabled: muteStore.Enabled(), Muted: muteStore.List()}
}

// notifySettingsHandler: GET the state; POST {"enabled": bool} turns
// notifications on or off, {"key": "<pk or group id>", "muted": bool} mutes
// or unmutes one conversation.
func notifySettingsHandler(w http.ResponseWriter, r *http.Request) {
	if muteStore == nil {
		http.Error(w, "notification settings unavailable", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, currentMute())
	case http.MethodPost:
		var body struct {
			Enabled *bool  `json:"enabled"`
			Key     string `json:"key"`
			Muted   bool   `json:"muted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var err error
		switch {
		case body.Enabled != nil:
			err = muteStore.SetEnabled(*body.Enabled)
		case body.Key != "":
			err = muteStore.SetMuted(body.Key, body.Muted)
		default:
			http.Error(w, "enabled or key required", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, currentMute())
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}
