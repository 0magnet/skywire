package mcpchannel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skycoin/skywire/pkg/cipher"
	skychatgroup "github.com/skycoin/skywire/pkg/skychat/group"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

const testGroupID = "15b5d6db-9ce0-409b-a2a4-78b99f046ca4"

// fakeVisor holds a group inbox and a DM event log the way the visor and
// skychat do: GroupWait and /events?wait= answer as soon as something is
// newer than the cursor.
type fakeVisor struct {
	mu     sync.Mutex
	cond   *sync.Cond
	inbox  []visorapi.GroupMessage
	dms    []dmEvent
	dmSeq  uint64
	sent   []visorapi.GroupSendArgs
	posted []map[string]any
	failN  int
}

func newFakeVisor() *fakeVisor {
	f := &fakeVisor{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *fakeVisor) groupPost(sender cipher.PubKey, text string, ts time.Time) {
	f.mu.Lock()
	f.inbox = append(f.inbox, visorapi.GroupMessage{GroupID: testGroupID, SenderPK: sender, Text: text, TS: ts, Arrived: time.Now()})
	f.mu.Unlock()
	f.cond.Broadcast()
	time.Sleep(2 * time.Millisecond) // keep Arrived strictly increasing
}

func (f *fakeVisor) dmPost(from cipher.PubKey, id, text string) {
	f.mu.Lock()
	f.dmSeq++
	f.dms = append(f.dms, dmEvent{Seq: f.dmSeq, ID: id, TS: time.Now(), Channel: "dm", Dir: "in", From: from.Hex(), Text: text})
	f.mu.Unlock()
	f.cond.Broadcast()
}

func (f *fakeVisor) after(since time.Time) []visorapi.GroupMessage {
	var out []visorapi.GroupMessage
	for _, m := range f.inbox {
		if m.Arrived.After(since) {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeVisor) waitFor(ready func() bool, wait time.Duration) {
	deadline := time.Now().Add(wait)
	go func() { time.Sleep(wait); f.cond.Broadcast() }()
	for !ready() && time.Now().Before(deadline) {
		f.cond.Wait()
	}
}

func (f *fakeVisor) GroupPoll(since time.Time) ([]visorapi.GroupMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.after(since), nil
}

func (f *fakeVisor) GroupWait(since time.Time, wait time.Duration) ([]visorapi.GroupMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failN > 0 {
		f.failN--
		return nil, errors.New("connection is shut down")
	}
	f.waitFor(func() bool { return len(f.after(since)) > 0 }, min(wait, 200*time.Millisecond))
	return f.after(since), nil
}

func (f *fakeVisor) GroupSend(args visorapi.GroupSendArgs) error {
	f.mu.Lock()
	f.sent = append(f.sent, args)
	f.mu.Unlock()
	f.groupPost(testPK(0), args.Text, time.Now())
	return nil
}

func (f *fakeVisor) GroupList() ([]visorapi.GroupInfo, error) {
	return []visorapi.GroupInfo{{ID: testGroupID, Name: "Skywire", Status: skychatgroup.StatusActive}}, nil
}

func (f *fakeVisor) GroupHistory(string, int) ([]visorapi.GroupMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]visorapi.GroupMessage(nil), f.inbox...), nil
}

func (f *fakeVisor) SkychatHTTP(in visorapi.SkychatHTTPIn) (visorapi.SkychatHTTPOut, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch in.Path {
	case "events":
		q, _ := url.ParseQuery(in.Query)                      //nolint:errcheck
		since, _ := strconv.ParseUint(q.Get("since"), 10, 64) //nolint:errcheck
		wait, _ := time.ParseDuration(q.Get("wait"))          //nolint:errcheck
		newer := func() []dmEvent {
			var out []dmEvent
			for _, ev := range f.dms {
				if ev.Seq > since {
					out = append(out, ev)
				}
			}
			return out
		}
		if wait > 0 {
			f.waitFor(func() bool { return len(newer()) > 0 }, min(wait, 200*time.Millisecond))
		}
		b, _ := json.Marshal(map[string]any{"head": f.dmSeq, "events": newer()}) //nolint:errcheck
		return visorapi.SkychatHTTPOut{Status: 200, Body: b}, nil
	case "message":
		var p map[string]any
		_ = json.Unmarshal(in.Body, &p) //nolint:errcheck
		f.posted = append(f.posted, p)
		return visorapi.SkychatHTTPOut{Status: 200, Body: []byte(`{}`)}, nil
	}
	return visorapi.SkychatHTTPOut{Status: 404}, nil
}

func testPK(i byte) cipher.PubKey {
	pk, _, _ := cipher.GenerateDeterministicKeyPair([]byte{i, 'k'}) //nolint:errcheck
	return pk
}

// mcpClient drives a Server over pipes the way Claude Code does.
type mcpClient struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan map[string]any
	cancel context.CancelFunc
	done   chan struct{}
	nextID int
}

func mcpClientStart(t *testing.T, conf Config) *mcpClient {
	t.Helper()
	srv := NewServer(conf)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	c := &mcpClient{t: t, in: inW, lines: make(chan map[string]any, 64), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		_ = srv.Serve(ctx, inR, outW) //nolint:errcheck
		_ = outW.Close()              //nolint:errcheck
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("server wrote a non-JSON line: %q", sc.Text())
				continue
			}
			c.lines <- m
		}
		close(c.lines)
	}()
	t.Cleanup(c.stop)
	resp := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	result := resp["result"].(map[string]any)
	caps := result["capabilities"].(map[string]any)
	if _, ok := caps["experimental"].(map[string]any)["claude/channel"]; !ok {
		t.Fatalf("initialize does not declare claude/channel: %v", caps)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return c
}

func (c *mcpClient) stop() {
	c.cancel()
	_ = c.in.Close() //nolint:errcheck
	<-c.done
}

func (c *mcpClient) send(v any) {
	b, _ := json.Marshal(v) //nolint:errcheck
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// call sends a request and returns its response, setting aside any
// notifications that arrive first.
func (c *mcpClient) call(method string, params any) map[string]any {
	c.nextID++
	id := float64(c.nextID)
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	for {
		select {
		case m := <-c.lines:
			if m["id"] == id {
				return m
			}
			go func() { c.lines <- m }()
		case <-time.After(5 * time.Second):
			c.t.Fatalf("no response to %s", method)
		}
	}
}

func (c *mcpClient) notification(wait time.Duration) (map[string]any, bool) {
	deadline := time.After(wait)
	for {
		select {
		case m := <-c.lines:
			if m["method"] == "notifications/claude/channel" {
				return m["params"].(map[string]any), true
			}
		case <-deadline:
			return nil, false
		}
	}
}

func (c *mcpClient) toolText(name string, args map[string]any) string {
	resp := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	result := resp["result"].(map[string]any)
	return result["content"].([]any)[0].(map[string]any)["text"].(string)
}

func testConfig(f *fakeVisor, state string) Config {
	return Config{
		VisorDial:    func() (Visor, error) { return f, nil },
		DMs:          true,
		IgnorePrefix: strings.TrimSpace(BotPrefix),
		ReplyPrefix:  true,
		StatePath:    state,
	}
}

func TestChannel_NotificationPerGroupMessage(t *testing.T) {
	f := newFakeVisor()
	f.groupPost(testPK(1), "old history", time.Now().Add(-time.Hour))
	c := mcpClientStart(t, testConfig(f, ""))
	if n, ok := c.notification(300 * time.Millisecond); ok {
		t.Fatalf("history was replayed: %v", n)
	}

	f.groupPost(testPK(1), "hello claude", time.Now())
	n, ok := c.notification(3 * time.Second)
	if !ok {
		t.Fatal("no notification for a new group message")
	}
	meta := n["meta"].(map[string]any)
	if n["content"] != "hello claude" || meta["chat_id"] != testGroupID || meta["kind"] != "group" ||
		meta["group_name"] != "Skywire" || meta["sender_pk"] != testPK(1).Hex() || meta["message_id"] == "" {
		t.Fatalf("notification = %v", n)
	}
	f.groupPost(testPK(2), "second", time.Now())
	if n, ok := c.notification(3 * time.Second); !ok || n["content"] != "second" {
		t.Fatalf("second notification = %v, %v", n, ok)
	}
}

func TestChannel_FiltersGroupsAndPrefix(t *testing.T) {
	f := newFakeVisor()
	conf := testConfig(f, "")
	conf.Groups = []string{"00000000-0000-0000-0000-000000000000"}
	c := mcpClientStart(t, conf)
	time.Sleep(50 * time.Millisecond)
	f.groupPost(testPK(1), "not my group", time.Now())
	if n, ok := c.notification(400 * time.Millisecond); ok {
		t.Fatalf("message from a group outside --groups delivered: %v", n)
	}

	f2 := newFakeVisor()
	c2 := mcpClientStart(t, testConfig(f2, ""))
	time.Sleep(50 * time.Millisecond)
	f2.groupPost(testPK(1), "[claude bot] another bot", time.Now())
	f2.groupPost(testPK(1), `{"skychat_delete":{"to_ts_nano":1}}`, time.Now())
	f2.groupPost(testPK(1), "real", time.Now())
	n, ok := c2.notification(3 * time.Second)
	if !ok || n["content"] != "real" {
		t.Fatalf("want only the real message, got %v", n)
	}
}

func TestChannel_ReplyPrefixAndNoEcho(t *testing.T) {
	f := newFakeVisor()
	c := mcpClientStart(t, testConfig(f, ""))
	time.Sleep(50 * time.Millisecond)
	f.groupPost(testPK(1), "question", time.Now())
	n, ok := c.notification(3 * time.Second)
	if !ok {
		t.Fatal("no notification")
	}
	msgID := n["meta"].(map[string]any)["message_id"].(string)

	if got := c.toolText("reply", map[string]any{"chat_id": testGroupID, "text": "answer", "reply_to": msgID}); got != "sent" {
		t.Fatalf("reply = %q", got)
	}
	f.mu.Lock()
	sent := f.sent[0].Text
	f.mu.Unlock()
	var env struct {
		Reply struct {
			ToSender string `json:"to_sender"`
			Text     string `json:"text"`
		} `json:"skychat_reply"`
	}
	if err := json.Unmarshal([]byte(sent), &env); err != nil || env.Reply.Text != "[claude bot] answer" || env.Reply.ToSender != testPK(1).Hex() {
		t.Fatalf("sent body = %s", sent)
	}
	if n, ok := c.notification(500 * time.Millisecond); ok {
		t.Fatalf("own reply echoed back: %v", n)
	}

	pk := testPK(3)
	if got := c.toolText("reply", map[string]any{"chat_id": pk.Hex(), "text": "hi dm"}); got != "sent" {
		t.Fatalf("dm reply = %q", got)
	}
	f.mu.Lock()
	posted := f.posted[0]
	f.mu.Unlock()
	if posted["recipient"] != pk.Hex() || posted["message"] != "[claude bot] hi dm" {
		t.Fatalf("dm post = %v", posted)
	}
}

func TestChannel_NoEchoWithoutPrefix(t *testing.T) {
	f := newFakeVisor()
	conf := testConfig(f, "")
	conf.ReplyPrefix = false
	conf.IgnorePrefix = ""
	c := mcpClientStart(t, conf)
	time.Sleep(50 * time.Millisecond)
	if got := c.toolText("reply", map[string]any{"chat_id": testGroupID, "text": "plain"}); got != "sent" {
		t.Fatalf("reply = %q", got)
	}
	if n, ok := c.notification(500 * time.Millisecond); ok {
		t.Fatalf("own reply echoed back: %v", n)
	}
}

func TestChannel_DMNotification(t *testing.T) {
	f := newFakeVisor()
	f.dmPost(testPK(4), "old", "before start")
	c := mcpClientStart(t, testConfig(f, ""))
	time.Sleep(100 * time.Millisecond)
	f.dmPost(testPK(4), "id-1", "dm hello")
	n, ok := c.notification(3 * time.Second)
	if !ok {
		t.Fatal("no DM notification")
	}
	meta := n["meta"].(map[string]any)
	if n["content"] != "dm hello" || meta["kind"] != "dm" || meta["chat_id"] != testPK(4).Hex() || meta["message_id"] != "id-1" {
		t.Fatalf("DM notification = %v", n)
	}
}

func TestChannel_ResumeWithoutReplay(t *testing.T) {
	f := newFakeVisor()
	state := filepath.Join(t.TempDir(), "state.json")
	c := mcpClientStart(t, testConfig(f, state))
	time.Sleep(50 * time.Millisecond)
	f.groupPost(testPK(1), "first", time.Now())
	f.dmPost(testPK(4), "d1", "dm first")
	for range 2 {
		if _, ok := c.notification(3 * time.Second); !ok {
			t.Fatal("missing a notification before restart")
		}
	}
	c.stop()
	time.Sleep(300 * time.Millisecond) // let the stopped server's last long-poll return

	f.groupPost(testPK(1), "while away", time.Now())
	c2 := mcpClientStart(t, testConfig(f, state))
	n, ok := c2.notification(3 * time.Second)
	if !ok || n["content"] != "while away" {
		t.Fatalf("after restart want only the missed message, got %v", n)
	}
	if n, ok := c2.notification(400 * time.Millisecond); ok {
		t.Fatalf("replayed after restart: %v", n)
	}

	// A visor restart refills its inbox with new arrival times.
	f.mu.Lock()
	for i := range f.inbox {
		f.inbox[i].Arrived = time.Now().Add(time.Duration(i) * time.Millisecond)
	}
	f.mu.Unlock()
	f.cond.Broadcast()
	if n, ok := c2.notification(500 * time.Millisecond); ok {
		t.Fatalf("visor inbox refill replayed: %v", n)
	}
}

func TestChannel_ReconnectsAfterDroppedSession(t *testing.T) {
	f := newFakeVisor()
	dials := 0
	var mu sync.Mutex
	conf := testConfig(f, "")
	conf.VisorDial = func() (Visor, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		return f, nil
	}
	c := mcpClientStart(t, conf)
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	f.failN = 1
	f.mu.Unlock()
	f.cond.Broadcast()
	time.Sleep(300 * time.Millisecond)
	f.groupPost(testPK(1), "after drop", time.Now())
	if n, ok := c.notification(4 * time.Second); !ok || n["content"] != "after drop" {
		t.Fatalf("no message after reconnect: %v", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if dials < 3 {
		t.Fatalf("dials = %d, want one per stream plus a redial after the failed call", dials)
	}
}

func TestChannel_ToolsListAndGroups(t *testing.T) {
	f := newFakeVisor()
	c := mcpClientStart(t, testConfig(f, ""))
	resp := c.call("tools/list", map[string]any{})
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %v", tools)
	}
	if got := c.toolText("groups", nil); got != testGroupID+" 0 Skywire\n" {
		t.Fatalf("groups = %q", got)
	}
	resp = c.call("no/such", map[string]any{})
	if resp["error"] == nil {
		t.Fatalf("unknown method answered without an error: %v", resp)
	}
}

func TestChannel_SendersAllowlist(t *testing.T) {
	f := newFakeVisor()
	conf := testConfig(f, "")
	conf.Senders = []string{testPK(2).Hex()}
	c := mcpClientStart(t, conf)
	time.Sleep(50 * time.Millisecond)
	f.groupPost(testPK(1), "stranger", time.Now())
	f.groupPost(testPK(2), "allowed", time.Now())
	n, ok := c.notification(3 * time.Second)
	if !ok || n["content"] != "allowed" {
		t.Fatalf("want only the allowed sender, got %v", n)
	}
}
