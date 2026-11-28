package cliskychat

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

// fakeGroupInbox hands out queued batches; GroupWait blocks until one exists.
type fakeGroupInbox struct {
	mu      sync.Mutex
	batches [][]visorapi.GroupMessage
	noWait  bool
	waits   []time.Time
	polls   []time.Time
}

func (f *fakeGroupInbox) next() []visorapi.GroupMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b
}

func (f *fakeGroupInbox) GroupPoll(since time.Time) ([]visorapi.GroupMessage, error) {
	f.mu.Lock()
	f.polls = append(f.polls, since)
	f.mu.Unlock()
	return f.next(), nil
}

func (f *fakeGroupInbox) GroupWait(since time.Time, _ time.Duration) ([]visorapi.GroupMessage, error) {
	if f.noWait {
		return nil, errors.New("gobrpc: can't find method app-visor.GroupWait")
	}
	f.mu.Lock()
	f.waits = append(f.waits, since)
	f.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	return f.next(), nil
}

func groupMsg(text string, ts, arrived time.Time) visorapi.GroupMessage {
	pk, _ := cipher.GenerateKeyPair()
	return visorapi.GroupMessage{GroupID: "g", SenderPK: pk, Text: text, TS: ts, Arrived: arrived}
}

func runGroupFollow(t *testing.T, inbox *fakeGroupInbox, since time.Time, want int) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []string
	groupInboxFollow(ctx, func() (groupInbox, error) { return inbox, nil }, since, time.Second, 5*time.Millisecond,
		func(m visorapi.GroupMessage) {
			got = append(got, m.Text)
			if len(got) == want {
				cancel()
			}
		})
	return got
}

// TestGroupInboxFollow prints the inbox newer than --since, then later
// arrivals by their arrival cursor, once each.
func TestGroupInboxFollow(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	old := groupMsg("old", t0, t0)
	fresh := groupMsg("fresh", t0.Add(time.Minute), t0.Add(time.Minute))
	later := groupMsg("later", t0.Add(2*time.Minute), t0.Add(2*time.Minute))
	backfill := groupMsg("backfill", t0.Add(-time.Hour), t0.Add(3*time.Minute))
	for _, noWait := range []bool{false, true} {
		inbox := &fakeGroupInbox{noWait: noWait, batches: [][]visorapi.GroupMessage{
			{old, fresh},
			{later, fresh},
			{backfill},
		}}
		got := runGroupFollow(t, inbox, t0.Add(time.Second), 3)
		require.Equal(t, []string{"fresh", "later", "backfill"}, got, "noWait=%v", noWait)
		cursors := inbox.waits
		if noWait {
			cursors = inbox.polls[1:]
		}
		require.Equal(t, fresh.Arrived, cursors[0], "the cursor is the newest arrival")
	}
}

// TestEventsThroughTheVisor reads skychat events as /events?wait= batches
// through the visor RPC, as `skychat events --via` does.
func TestEventsThroughTheVisor(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	prevAddr, prevClient, prevTransport, prevChat := httpAddr, visorClient, chatTransport, chatClient
	t.Cleanup(func() {
		httpAddr, visorClient, chatTransport, chatClient = prevAddr, prevClient, prevTransport, prevChat
	})
	httpAddr = addr
	chatTransport = &visorTransport{next: http.DefaultTransport}
	chatClient = &http.Client{Transport: chatTransport}
	fv := &fakeVisor{}
	visorClient = func() (visorapi.API, error) { return &eventsVisor{fakeVisor: fv}, nil }

	var out bytes.Buffer
	var lastSeq uint64 = 3
	seen := newIDDedupe(16)
	require.NoError(t, waitEventsOnce(context.Background(), &out, true, "dm", &lastSeq, seen))
	require.Equal(t, uint64(5), lastSeq)
	require.Equal(t, 2, strings.Count(out.String(), "\n"))
	require.Contains(t, out.String(), `"id":"b"`)

	_, err = chatClient.Get("http://" + addr + "/events?since=0") //nolint:bodyclose,noctx
	require.Error(t, err, "the stream form still cannot go through the visor")
}

type eventsVisor struct{ *fakeVisor }

func (e *eventsVisor) SkychatHTTP(in visorapi.SkychatHTTPIn) (visorapi.SkychatHTTPOut, error) {
	if in.Path != "events" || !strings.Contains(in.Query, "since=3") || !strings.Contains(in.Query, "wait=20s") || !strings.Contains(in.Query, "channel=dm") {
		return visorapi.SkychatHTTPOut{Status: 400}, nil
	}
	body := `{"head":5,"events":[{"seq":4,"id":"a","channel":"dm"},{"seq":5,"id":"b","channel":"dm"}]}`
	return visorapi.SkychatHTTPOut{Status: 200, Body: []byte(body)}, nil
}
