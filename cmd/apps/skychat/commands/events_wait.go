// Package commands cmd/apps/skychat/commands/events_wait.go c4-app-chat
//
// GET /events?wait=<duration> is the buffered form of /events: it waits for
// the first event after since, then answers with that batch as one JSON
// object. The visor's skychat RPC proxy buffers responses, so this is the
// form that reaches a client over --via or a portless skychat.
package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	// eventsWaitMax keeps a long-poll under the 30 s timeout of the visor's
	// skychat proxy (pkg/visor/api_skychat.go SkychatProxy).
	eventsWaitMax = 20 * time.Second
	// eventsWaitBatchMax bounds one response; the client asks again from
	// the last seq it got.
	eventsWaitBatchMax = eventSubBufSize
)

// eventsWaitResponse is the /events?wait= answer. Head is the newest seq the
// hub has assigned. A head below the client's since means skychat restarted
// and its seq began again, so the client starts over from since=0.
type eventsWaitResponse struct {
	Head   uint64      `json:"head"`
	Events []chatEvent `json:"events"`
}

func (h *sseHub) eventsHeadSeq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

func eventsWaitServe(w http.ResponseWriter, req *http.Request, waitText string) {
	wait, err := time.ParseDuration(waitText)
	if err != nil || wait < 0 {
		http.Error(w, "invalid wait", http.StatusBadRequest)
		return
	}
	wait = min(wait, eventsWaitMax)
	var since uint64
	if v := req.URL.Query().Get("since"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &since); err != nil {
			http.Error(w, "invalid since", http.StatusBadRequest)
			return
		}
	}
	channels := parseEventChannels(req.URL.Query().Get("channel"))
	ch, unsubscribe := hub.subscribeEvents(channels, since)
	defer unsubscribe()

	resp := eventsWaitResponse{Head: hub.eventsHeadSeq(), Events: []chatEvent{}}
	if resp.Head >= since && wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case ev := <-ch:
			resp.Events = append(resp.Events, ev)
		case <-timer.C:
		case <-req.Context().Done():
		}
		timer.Stop()
	}
	if resp.Head >= since {
	drain:
		for len(resp.Events) < eventsWaitBatchMax {
			select {
			case ev := <-ch:
				resp.Events = append(resp.Events, ev)
			default:
				break drain
			}
		}
	}
	for _, ev := range resp.Events {
		resp.Head = max(resp.Head, ev.Seq)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		chatLog.Debugf("events wait write failed: %v", err)
	}
}
