package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func eventsWaitGet(t *testing.T, query string) eventsWaitResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	eventsHandler(rr, httptest.NewRequest(http.MethodGet, "/events?"+query, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rr.Code, rr.Body.String())
	}
	var resp eventsWaitResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestEventsWait_ReturnsBufferedAfterSince(t *testing.T) {
	withLifecycleEnv(t)
	hub.recordEvent(chatEvent{ID: "a", Channel: channelDM, Dir: "in", Text: "one"})
	hub.recordEvent(chatEvent{ID: "b", Channel: channelDM, Dir: "in", Text: "two"})

	resp := eventsWaitGet(t, "since=1&wait=0s")
	if resp.Head != 2 || len(resp.Events) != 1 || resp.Events[0].ID != "b" {
		t.Fatalf("got %+v, want head 2 and only event b", resp)
	}
}

func TestEventsWait_WaitsForNextEvent(t *testing.T) {
	withLifecycleEnv(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		hub.recordEvent(chatEvent{ID: "late", Channel: channelGroup, Dir: "in", Text: "hi"})
	}()
	start := time.Now()
	resp := eventsWaitGet(t, "since=0&wait=5s")
	if len(resp.Events) != 1 || resp.Events[0].ID != "late" || resp.Head != 1 {
		t.Fatalf("got %+v, want the late event", resp)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("waited the whole timeout instead of returning on the event")
	}
}

func TestEventsWait_HeadBelowSinceAnswersAtOnce(t *testing.T) {
	withLifecycleEnv(t)
	start := time.Now()
	resp := eventsWaitGet(t, "since=500&wait=5s")
	if resp.Head != 0 || len(resp.Events) != 0 {
		t.Fatalf("got %+v, want head 0 and no events", resp)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a restarted seq must answer at once")
	}
}

func TestEventsWait_RejectsBadWait(t *testing.T) {
	withLifecycleEnv(t)
	rr := httptest.NewRecorder()
	eventsHandler(rr, httptest.NewRequest(http.MethodGet, "/events?wait=soon", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}
