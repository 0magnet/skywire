// Package bridge pkg/skychat/bridge/telegram_test.go c3-app-chat
package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken  = "123456:SECRET-token"
	testChatID = int64(-1001)
	testSelfPK = "0281a102c82820e811368c8d028cf11b1a985043b726b1bcdb8fce89b27384b2cb"
	testPeerPK = "03aaaaaaaa00000000000000000000000000000000000000000000000000bbbb"
)

// fakeTelegram serves getUpdates and sendMessage for one bot.
type fakeTelegram struct {
	mu       sync.Mutex
	updates  []TelegramUpdate
	sent     []string
	offsets  []int64
	limitHit int // answer 429 to this many sendMessage calls
	srv      *httptest.Server
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+testToken+"/") {
			http.Error(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var params map[string]any
		_ = json.NewDecoder(r.Body).Decode(&params) //nolint:errcheck
		switch strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/") {
		case "getUpdates":
			offset := int64(params["offset"].(float64))
			f.mu.Lock()
			f.offsets = append(f.offsets, offset)
			var out []TelegramUpdate
			for _, u := range f.updates {
				if u.UpdateID >= offset {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			if len(out) == 0 {
				time.Sleep(20 * time.Millisecond)
			}
			writeJSON(w, map[string]any{"ok": true, "result": out})
		case "sendMessage":
			if int64(params["chat_id"].(float64)) != testChatID {
				writeJSON(w, map[string]any{"ok": false, "error_code": 400, "description": "chat not found"})
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.limitHit > 0 {
				f.limitHit--
				writeJSON(w, map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests", "parameters": map[string]any{"retry_after": 1}})
				return
			}
			f.sent = append(f.sent, params["text"].(string))
			writeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func (f *fakeTelegram) push(u TelegramUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, u)
}

func (f *fakeTelegram) sentLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lines []string
	for _, s := range f.sent {
		lines = append(lines, strings.Split(s, "\n")...)
	}
	return lines
}

func (f *fakeTelegram) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// fakeGroup is a skychat group that, like the visor, shows the bridge its
// own posts on the stream.
type fakeGroup struct {
	mu      sync.Mutex
	history []GroupMessage
	sent    []string
	subs    []chan GroupMessage
	clock   int64
}

func (g *fakeGroup) post(pk, body string) {
	g.mu.Lock()
	g.clock++
	m := GroupMessage{TimestampNs: g.clock, SenderPK: pk, Body: body}
	g.history = append(g.history, m)
	subs := append([]chan GroupMessage(nil), g.subs...)
	g.mu.Unlock()
	for _, ch := range subs {
		ch <- m
	}
}

func (g *fakeGroup) Send(_ context.Context, text string) error {
	g.mu.Lock()
	g.sent = append(g.sent, text)
	g.mu.Unlock()
	g.post(testSelfPK, text)
	return nil
}

func (g *fakeGroup) Stream(ctx context.Context, sinceNs int64, deliver func(GroupMessage)) error {
	ch := make(chan GroupMessage, 64)
	g.mu.Lock()
	var backlog []GroupMessage
	if sinceNs > 0 {
		for _, m := range g.history {
			if m.TimestampNs > sinceNs {
				backlog = append(backlog, m)
			}
		}
	}
	g.subs = append(g.subs, ch)
	g.mu.Unlock()
	for _, m := range backlog {
		deliver(m)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-ch:
			deliver(m)
		}
	}
}

func (g *fakeGroup) SelfPK(context.Context) (string, error) { return testSelfPK, nil }

func (g *fakeGroup) ProfileName(_ context.Context, pk string) (string, error) {
	if pk == testPeerPK {
		return "Alice", nil
	}
	return "", nil
}

func (g *fakeGroup) sentTexts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sent...)
}

func (g *fakeGroup) subscribed() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.subs)
}

func testBridge(t *testing.T, tg *fakeTelegram, g *fakeGroup, statePath string, interval time.Duration) (*Telegram, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	b, err := TelegramNew(TelegramConfig{
		API:              &TelegramAPI{BaseURL: tg.srv.URL, Token: testToken},
		ChatID:           testChatID,
		Group:            g,
		StatePath:        statePath,
		TelegramInterval: interval,
		SkychatInterval:  10 * time.Millisecond,
		PollTimeout:      time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.Run(ctx)
	}()
	waitFor(t, "stream subscribed", func() bool { return g.subscribed() > 0 })
	return b, cancel, &wg
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func tgText(id int64, from TelegramUser, text string) TelegramUpdate {
	return TelegramUpdate{UpdateID: id, Message: &TelegramMessage{
		MessageID: id, From: &from, Chat: TelegramChat{ID: testChatID}, Text: text,
	}}
}

func TestTelegramRelaysBothWays(t *testing.T) {
	tg := newFakeTelegram(t)
	g := &fakeGroup{}
	_, cancel, wg := testBridge(t, tg, g, filepath.Join(t.TempDir(), "state.json"), 10*time.Millisecond)
	defer func() { cancel(); wg.Wait() }()

	ann := TelegramUser{ID: 1, FirstName: "Ann", LastName: "Lee"}
	tg.push(tgText(10, ann, "/start"))
	tg.push(tgText(11, ann, "hello group"))
	photo := TelegramUpdate{UpdateID: 12, Message: &TelegramMessage{
		From: &TelegramUser{ID: 2, Username: "bob"}, Chat: TelegramChat{ID: testChatID},
		Photo: json.RawMessage(`[{"file_id":"x"}]`), Caption: "look",
	}}
	tg.push(photo)
	other := tgText(13, ann, "wrong chat")
	other.Message.Chat.ID = 42
	tg.push(other)

	waitFor(t, "telegram lines in the group", func() bool {
		return strings.Contains(strings.Join(g.sentTexts(), "\n"), "[photo]")
	})
	got := strings.Join(g.sentTexts(), "\n")
	for _, want := range []string{"[TG] Ann Lee: hello group", "[TG] @bob: [photo] look"} {
		if !strings.Contains(got, want) {
			t.Errorf("group got %q, missing %q", got, want)
		}
	}
	for _, bad := range []string{"/start", "wrong chat"} {
		if strings.Contains(got, bad) {
			t.Errorf("group got %q, should not contain %q", got, bad)
		}
	}

	g.post(testPeerPK, "hi from skychat")
	g.post(testSelfPK, "[claude bot] the host posting itself")
	waitFor(t, "group lines in telegram", func() bool { return len(tg.sentLines()) >= 2 })
	time.Sleep(100 * time.Millisecond)
	lines := tg.sentLines()
	want := []string{
		"[skychat] Alice: hi from skychat",
		"[skychat] " + PKShort(testSelfPK) + ": [claude bot] the host posting itself",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("telegram got %q, want %q (an echoed [TG] line would be a loop)", lines, want)
	}
}

func TestTelegramRateLimitQueues(t *testing.T) {
	tg := newFakeTelegram(t)
	tg.limitHit = 1
	g := &fakeGroup{}
	b, cancel, wg := testBridge(t, tg, g, filepath.Join(t.TempDir(), "state.json"), 300*time.Millisecond)
	defer func() { cancel(); wg.Wait() }()

	for i := 0; i < 6; i++ {
		g.post(testPeerPK, "line "+string(rune('a'+i)))
	}
	waitFor(t, "all lines delivered", func() bool { return len(tg.sentLines()) == 6 })
	if n := tg.sendCount(); n >= 6 {
		t.Errorf("sent %d messages for 6 lines, want them joined", n)
	}
	for i, l := range tg.sentLines() {
		if want := "[skychat] Alice: line " + string(rune('a'+i)); l != want {
			t.Errorf("line %d = %q, want %q", i, l, want)
		}
	}
	if q := b.State().ToTelegram; len(q) != 0 {
		t.Errorf("queue holds %d lines after delivery", len(q))
	}
}

func TestTelegramRestartResume(t *testing.T) {
	tg := newFakeTelegram(t)
	g := &fakeGroup{}
	statePath := filepath.Join(t.TempDir(), "state.json")
	ann := TelegramUser{ID: 1, FirstName: "Ann"}

	_, cancel, wg := testBridge(t, tg, g, statePath, 10*time.Millisecond)
	tg.push(tgText(1, ann, "first from tg"))
	g.post(testPeerPK, "first from skychat")
	waitFor(t, "first round", func() bool { return len(tg.sentLines()) == 1 && len(g.sentTexts()) == 1 })
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()

	// While the bridge is down, one message lands on each side.
	g.mu.Lock()
	g.subs = nil
	g.mu.Unlock()
	g.post(testPeerPK, "while down")
	tg.push(tgText(2, ann, "second from tg"))

	_, cancel, wg = testBridge(t, tg, g, statePath, 10*time.Millisecond)
	defer func() { cancel(); wg.Wait() }()
	waitFor(t, "second round", func() bool { return len(tg.sentLines()) == 2 && len(g.sentTexts()) == 2 })
	time.Sleep(200 * time.Millisecond)

	if got, want := strings.Join(tg.sentLines(), "\n"), "[skychat] Alice: first from skychat\n[skychat] Alice: while down"; got != want {
		t.Errorf("telegram got %q, want %q", got, want)
	}
	if got, want := strings.Join(g.sentTexts(), "\n"), "[TG] Ann: first from tg\n[TG] Ann: second from tg"; got != want {
		t.Errorf("group got %q, want %q", got, want)
	}
}

func TestTelegramQueueOverflowDropsOldest(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	b, err := TelegramNew(TelegramConfig{
		API: &TelegramAPI{Token: testToken}, ChatID: testChatID, Group: &fakeGroup{},
		StatePath: statePath, QueueLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		b.skychatAccept(GroupMessage{TimestampNs: i, SenderPK: testPeerPK, Body: string(rune('a' + i - 1))})
	}
	q := b.State().ToTelegram
	if len(q) != 2 || q[0].Text != "b" || q[1].Text != "c" || b.telegramDropped != 1 {
		t.Fatalf("queue %+v dropped %d, want b and c with one dropped", q, b.telegramDropped)
	}
	saved, err := StateLoad(statePath)
	if err != nil || len(saved.ToTelegram) != 2 || saved.SkychatSinceNs != 3 {
		t.Fatalf("saved state %+v, %v", saved, err)
	}
	if got := sentHead(2, 1, 2); got != 1 {
		t.Errorf("sentHead after one drop = %d, want 1", got)
	}
}

func TestTelegramFormat(t *testing.T) {
	bot := &TelegramUser{ID: 9, Bot: true, FirstName: "bridge"}
	ann := &TelegramUser{ID: 1, FirstName: "Ann"}
	cases := []struct {
		name string
		msg  TelegramMessage
		want string
		ok   bool
	}{
		{"command", TelegramMessage{From: ann, Text: "/help"}, "", false},
		{"text", TelegramMessage{From: ann, Text: "hi"}, "[TG] Ann: hi", true},
		{"username", TelegramMessage{From: &TelegramUser{Username: "zed"}, Text: "hi"}, "[TG] @zed: hi", true},
		{"sticker", TelegramMessage{From: ann, Sticker: &struct {
			Emoji string `json:"emoji"`
		}{"😀"}}, "[TG] Ann: [sticker 😀]", true},
		{"voice", TelegramMessage{From: ann, Voice: json.RawMessage(`{}`)}, "[TG] Ann: [voice]", true},
		{"service", TelegramMessage{From: ann}, "", false},
		{"reply", TelegramMessage{From: ann, Text: "yes", ReplyTo: &TelegramMessage{
			From: &TelegramUser{FirstName: "Bob"}, Text: "a question that goes on for quite a while longer than forty",
		}}, "[TG] Ann (reply to Bob: a question that goes on for quite a whi…): yes", true},
		{"reply to skychat", TelegramMessage{From: ann, Text: "ok", ReplyTo: &TelegramMessage{
			From: bot, Text: "[skychat] Alice: ship it",
		}}, "[TG] Ann (reply to Alice: ship it): ok", true},
		{"topic root", TelegramMessage{From: ann, Text: "in topic", ReplyTo: &TelegramMessage{
			From: ann, TopicCreated: json.RawMessage(`{"name":"t"}`),
		}}, "[TG] Ann: in topic", true},
	}
	for _, c := range cases {
		got, ok := TelegramFormat(&c.msg)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestTelegramAPIErrorsHideToken(t *testing.T) {
	api := &TelegramAPI{BaseURL: "http://127.0.0.1:1", Token: testToken}
	err := api.MessageSend(context.Background(), testChatID, "x")
	if err == nil {
		t.Fatal("want an error from a closed port")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks the token: %v", err)
	}
}
