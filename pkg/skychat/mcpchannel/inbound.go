// Package mcpchannel pkg/skychat/mcpchannel/inbound.go c3-app-chat
package mcpchannel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	skychatgroup "github.com/skycoin/skywire/pkg/skychat/group"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

const (
	// groupWaitTime is one GroupWait long-poll; the visor caps it at 20 s.
	groupWaitTime = 20 * time.Second
	// dmWaitTime is one /events?wait= long-poll; skychat caps it at 20 s.
	dmWaitTime = 20 * time.Second
	// seenWindow is how far before the newest delivered message a late
	// arrival is still delivered. A visor restart refills its group inbox
	// with new arrival times, so arrival alone cannot tell new from old.
	seenWindow = 10 * time.Minute
	seenMax    = 4096
	// sentEchoWindow is how long a posted reply is recognized when the
	// visor hands it back.
	sentEchoWindow = 5 * time.Minute
	recentMax      = 512
	contentMax     = 8000
	reconnectMin   = time.Second
	reconnectMax   = 30 * time.Second
	groupListAge   = time.Minute
)

// channelState is what the channel keeps across restarts so it resumes
// without replaying history.
type channelState struct {
	GroupArrived time.Time        `json:"group_arrived"`
	GroupNewest  time.Time        `json:"group_newest"`
	GroupSeen    map[string]int64 `json:"group_seen,omitempty"`
	DMSeq        uint64           `json:"dm_seq"`
	DMNewest     time.Time        `json:"dm_newest"`
	DMSeen       map[string]int64 `json:"dm_seen,omitempty"`
}

// messageRef is a recent message a reply can quote.
type messageRef struct {
	sender string
	ts     time.Time
	text   string
}

type inbound struct {
	s *Server

	mu     sync.Mutex
	state  channelState
	recent map[string]messageRef
	order  []string
	sent   map[string]time.Time

	groupsMu      sync.Mutex
	groupNames    map[string]string
	groupsActive  map[string]bool
	groupsFetched time.Time
}

func newInbound(s *Server) *inbound {
	in := &inbound{
		s:      s,
		recent: make(map[string]messageRef),
		sent:   make(map[string]time.Time),
	}
	in.stateLoad()
	return in
}

func (in *inbound) stateLoad() {
	in.state = channelState{GroupSeen: map[string]int64{}, DMSeen: map[string]int64{}}
	if in.s.conf.StatePath == "" {
		return
	}
	b, err := os.ReadFile(in.s.conf.StatePath)
	if err != nil {
		return
	}
	var st channelState
	if err := json.Unmarshal(b, &st); err != nil {
		in.s.conf.Logf("skychat channel: state %s unreadable, starting fresh: %v", in.s.conf.StatePath, err)
		return
	}
	if st.GroupSeen == nil {
		st.GroupSeen = map[string]int64{}
	}
	if st.DMSeen == nil {
		st.DMSeen = map[string]int64{}
	}
	in.state = st
}

// stateSave writes the state; the caller holds in.mu.
func (in *inbound) stateSave() {
	path := in.s.conf.StatePath
	if path == "" {
		return
	}
	b, err := json.Marshal(in.state)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		in.s.conf.Logf("skychat channel: state dir: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		in.s.conf.Logf("skychat channel: state write: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		in.s.conf.Logf("skychat channel: state rename: %v", err)
	}
}

func (in *inbound) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); in.groupLoop(ctx) }()
	if in.s.conf.DMs {
		wg.Add(1)
		go func() { defer wg.Done(); in.dmLoop(ctx) }()
	}
	wg.Wait()
}

// backoff sleeps for *delay and doubles it; false means ctx is done.
func backoff(ctx context.Context, delay *time.Duration) bool {
	t := time.NewTimer(*delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	}
	*delay = min(*delay*2, reconnectMax)
	return true
}

func (in *inbound) groupLoop(ctx context.Context) {
	delay := reconnectMin
	failing := false
	for ctx.Err() == nil {
		in.mu.Lock()
		since := in.state.GroupArrived
		in.mu.Unlock()
		var msgs []visorapi.GroupMessage
		err := in.s.groupLink.call(func(v Visor) error {
			var err error
			if since.IsZero() {
				msgs, err = v.GroupPoll(time.Time{})
			} else {
				msgs, err = v.GroupWait(since, groupWaitTime)
			}
			return err
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !failing {
				in.s.conf.Logf("skychat channel: group stream: %v (reconnecting)", err)
				failing = true
			}
			if !backoff(ctx, &delay) {
				return
			}
			continue
		}
		if failing {
			in.s.conf.Logf("skychat channel: group stream reconnected")
			failing = false
		}
		delay = reconnectMin
		if since.IsZero() {
			in.groupStart(msgs)
			continue
		}
		in.groupDeliver(msgs)
	}
}

// groupStart takes the visor's current inbox as already seen, so a first
// run starts from now instead of replaying it.
func (in *inbound) groupStart(msgs []visorapi.GroupMessage) {
	in.mu.Lock()
	defer in.mu.Unlock()
	arrived := time.Now()
	for _, m := range msgs {
		if m.Arrived.After(in.state.GroupArrived) {
			in.state.GroupArrived = m.Arrived
		}
		if m.TS.After(in.state.GroupNewest) {
			in.state.GroupNewest = m.TS
		}
	}
	for _, m := range msgs {
		if in.state.GroupNewest.Sub(m.TS) < seenWindow {
			in.state.GroupSeen[groupMessageKey(m)] = m.TS.UnixNano()
		}
	}
	if in.state.GroupArrived.IsZero() {
		// An empty inbox: any arrival from now on is new.
		in.state.GroupArrived = arrived.Add(-time.Second)
	}
	if in.state.GroupNewest.IsZero() {
		in.state.GroupNewest = arrived
	}
	in.stateSave()
}

func groupMessageKey(m visorapi.GroupMessage) string {
	return m.GroupID + "/" + m.SenderPK.Hex() + "/" + strconv.FormatInt(m.TS.UnixNano(), 10)
}

func (in *inbound) groupDeliver(msgs []visorapi.GroupMessage) {
	if len(msgs) == 0 {
		return
	}
	type pending struct {
		m    visorapi.GroupMessage
		body messageBody
	}
	var out []pending
	in.mu.Lock()
	for _, m := range msgs {
		if m.Arrived.After(in.state.GroupArrived) {
			in.state.GroupArrived = m.Arrived
		}
		key := groupMessageKey(m)
		if _, seen := in.state.GroupSeen[key]; seen {
			continue
		}
		if in.state.GroupNewest.Sub(m.TS) >= seenWindow {
			continue
		}
		in.state.GroupSeen[key] = m.TS.UnixNano()
		if m.TS.After(in.state.GroupNewest) {
			in.state.GroupNewest = m.TS
		}
		if in.sentEcho(m.GroupID, m.Text) {
			continue
		}
		body, ok := messageBodyParse(m.Text)
		if !ok || in.ignored(body.text) || !in.senderAllowed(m.SenderPK.Hex()) {
			continue
		}
		out = append(out, pending{m: m, body: body})
	}
	seenPrune(in.state.GroupSeen, in.state.GroupNewest)
	in.stateSave()
	in.mu.Unlock()

	for _, p := range out {
		name, ok := in.groupWanted(p.m.GroupID)
		if !ok {
			continue
		}
		id := strconv.FormatInt(p.m.TS.UnixNano(), 10)
		in.recentAdd(p.m.GroupID+"/"+id, messageRef{sender: p.m.SenderPK.Hex(), ts: p.m.TS, text: p.body.text})
		meta := map[string]string{
			"chat_id":    p.m.GroupID,
			"kind":       "group",
			"group_name": name,
			"sender_pk":  p.m.SenderPK.Hex(),
			"message_id": id,
			"ts":         p.m.TS.UTC().Format(time.RFC3339),
		}
		p.body.metaFill(meta)
		in.s.channelNotify(contentBound(p.body.text), meta)
	}
}

// seenPrune drops keys older than the window and bounds the set.
func seenPrune(seen map[string]int64, newest time.Time) {
	floor := newest.Add(-seenWindow).UnixNano()
	for k, ts := range seen {
		if ts < floor {
			delete(seen, k)
		}
	}
	for k := range seen {
		if len(seen) <= seenMax {
			break
		}
		delete(seen, k)
	}
}

// groupWanted reports whether messages from id are delivered, with the
// group's name. The joined-group list is refreshed when an unknown id shows up.
func (in *inbound) groupWanted(id string) (string, bool) {
	in.groupsMu.Lock()
	name, known := in.groupNames[id]
	stale := time.Since(in.groupsFetched) > groupListAge
	in.groupsMu.Unlock()
	if !known && stale {
		in.groupsRefresh()
		in.groupsMu.Lock()
		name = in.groupNames[id]
		in.groupsMu.Unlock()
	}
	if len(in.s.conf.Groups) > 0 {
		for _, g := range in.s.conf.Groups {
			if g == id {
				return name, true
			}
		}
		return "", false
	}
	in.groupsMu.Lock()
	defer in.groupsMu.Unlock()
	return name, in.groupsActive[id]
}

func (in *inbound) groupsRefresh() {
	var list []visorapi.GroupInfo
	err := in.s.groupLink.call(func(v Visor) error {
		var err error
		list, err = v.GroupList()
		return err
	})
	in.groupsMu.Lock()
	defer in.groupsMu.Unlock()
	in.groupsFetched = time.Now()
	if err != nil {
		in.s.conf.Logf("skychat channel: group list: %v", err)
		return
	}
	in.groupNames = make(map[string]string, len(list))
	in.groupsActive = make(map[string]bool, len(list))
	for _, g := range list {
		in.groupNames[g.ID] = g.Name
		in.groupsActive[g.ID] = g.Status == skychatgroup.StatusActive
	}
}

// dmEvent is the part of skychat's /events record the channel reads.
type dmEvent struct {
	Seq       uint64    `json:"seq"`
	ID        string    `json:"id"`
	TS        time.Time `json:"ts"`
	Channel   string    `json:"channel"`
	Dir       string    `json:"dir"`
	From      string    `json:"from"`
	Text      string    `json:"text"`
	ReplyToID string    `json:"reply_to_id"`
	FileName  string    `json:"file_name"`
	FileSize  int64     `json:"file_size"`
	Request   bool      `json:"request"`
}

type dmEventsBatch struct {
	Head   *uint64   `json:"head"`
	Events []dmEvent `json:"events"`
}

var errDMWaitUnsupported = errors.New("skychat /events has no wait mode")

func (in *inbound) dmEventsWait(since uint64, wait time.Duration) (dmEventsBatch, error) {
	var batch dmEventsBatch
	err := in.s.dmLink.call(func(v Visor) error {
		out, err := v.SkychatHTTP(visorapi.SkychatHTTPIn{
			Method: "GET",
			Path:   "events",
			Query:  fmt.Sprintf("channel=dm&since=%d&wait=%s", since, wait),
		})
		if err != nil {
			return err
		}
		if out.Status != 200 {
			return fmt.Errorf("skychat /events: HTTP %d: %s", out.Status, strings.TrimSpace(string(out.Body)))
		}
		return json.Unmarshal(out.Body, &batch)
	})
	if err == nil && batch.Head == nil {
		err = errDMWaitUnsupported
	}
	return batch, err
}

func (in *inbound) dmLoop(ctx context.Context) {
	delay := reconnectMin
	failing := false
	in.mu.Lock()
	fresh := in.state.DMNewest.IsZero()
	in.mu.Unlock()
	answered := false
	for ctx.Err() == nil {
		in.mu.Lock()
		since := in.state.DMSeq
		in.mu.Unlock()
		wait := dmWaitTime
		if fresh {
			wait = 0
		}
		batch, err := in.dmEventsWait(since, wait)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !answered && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errDMWaitUnsupported)) {
				// An older skychat streams /events instead of answering, and each
				// try would hold a request open on the visor.
				in.s.conf.Logf("skychat channel: DMs off, skychat on that visor has no /events wait mode (update it): %v", err)
				return
			}
			if !failing {
				in.s.conf.Logf("skychat channel: dm stream: %v (reconnecting)", err)
				failing = true
			}
			if !backoff(ctx, &delay) {
				return
			}
			continue
		}
		if failing {
			in.s.conf.Logf("skychat channel: dm stream reconnected")
			failing = false
		}
		answered = true
		delay = reconnectMin
		if *batch.Head < since {
			// skychat restarted and its seq began again.
			in.mu.Lock()
			in.state.DMSeq = 0
			in.mu.Unlock()
			continue
		}
		if fresh {
			in.dmStart(batch)
			fresh = false
			continue
		}
		in.dmDeliver(batch)
	}
}

// dmStart skips what skychat already holds, so a first run starts from now.
func (in *inbound) dmStart(batch dmEventsBatch) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.state.DMSeq = *batch.Head
	in.state.DMNewest = time.Now()
	for _, ev := range batch.Events {
		if ev.TS.After(in.state.DMNewest) {
			in.state.DMNewest = ev.TS
		}
	}
	in.stateSave()
}

func (in *inbound) dmDeliver(batch dmEventsBatch) {
	var out []dmEvent
	in.mu.Lock()
	for _, ev := range batch.Events {
		if ev.Seq > in.state.DMSeq {
			in.state.DMSeq = ev.Seq
		}
		if ev.Channel != "dm" || ev.Dir != "in" || ev.ID == "" {
			continue
		}
		if _, seen := in.state.DMSeen[ev.ID]; seen {
			continue
		}
		if ev.TS.Before(in.state.DMNewest.Add(-seenWindow)) {
			continue
		}
		in.state.DMSeen[ev.ID] = ev.TS.UnixNano()
		if ev.TS.After(in.state.DMNewest) {
			in.state.DMNewest = ev.TS
		}
		out = append(out, ev)
	}
	seenPrune(in.state.DMSeen, in.state.DMNewest)
	in.stateSave()
	in.mu.Unlock()

	for _, ev := range out {
		body, ok := messageBodyParse(ev.Text)
		if !ok {
			continue
		}
		if ev.FileName != "" {
			body.text = fmt.Sprintf("[file] %s (%d bytes)", ev.FileName, ev.FileSize)
		}
		if in.ignored(body.text) || !in.senderAllowed(ev.From) {
			continue
		}
		in.recentAdd(ev.From+"/"+ev.ID, messageRef{sender: ev.From, ts: ev.TS, text: body.text})
		meta := map[string]string{
			"chat_id":    ev.From,
			"kind":       "dm",
			"message_id": ev.ID,
			"ts":         ev.TS.UTC().Format(time.RFC3339),
		}
		body.metaFill(meta)
		if ev.ReplyToID != "" {
			meta["reply_to"] = ev.ReplyToID
		}
		if ev.Request {
			meta["request"] = "true"
		}
		in.s.channelNotify(contentBound(body.text), meta)
	}
}

func (in *inbound) senderAllowed(pk string) bool {
	if len(in.s.conf.Senders) == 0 {
		return true
	}
	for _, s := range in.s.conf.Senders {
		if strings.EqualFold(strings.TrimSpace(s), pk) {
			return true
		}
	}
	return false
}

func (in *inbound) ignored(text string) bool {
	p := strings.TrimSpace(in.s.conf.IgnorePrefix)
	return p != "" && strings.HasPrefix(strings.TrimSpace(text), p)
}

// sentRecord remembers a body the channel posted; sentEcho matches it when
// the visor hands it back. The caller of sentEcho holds in.mu.
func (in *inbound) sentRecord(chatID, body string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	now := time.Now()
	for k, t := range in.sent {
		if now.Sub(t) > sentEchoWindow {
			delete(in.sent, k)
		}
	}
	in.sent[chatID+"\x00"+body] = now
}

func (in *inbound) sentEcho(chatID, body string) bool {
	k := chatID + "\x00" + body
	if _, ok := in.sent[k]; ok {
		delete(in.sent, k)
		return true
	}
	return false
}

func (in *inbound) recentAdd(key string, ref messageRef) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, ok := in.recent[key]; !ok {
		in.order = append(in.order, key)
	}
	in.recent[key] = ref
	if len(in.order) > recentMax {
		drop := in.order[:len(in.order)-recentMax]
		for _, k := range drop {
			delete(in.recent, k)
		}
		in.order = append([]string(nil), in.order[len(drop):]...)
	}
}

func (in *inbound) recentGet(key string) (messageRef, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	ref, ok := in.recent[key]
	return ref, ok
}

func contentBound(text string) string {
	if len(text) <= contentMax {
		return text
	}
	cut := contentMax
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + " …[truncated]"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// messageBody is a message text with skychat's envelopes unwrapped.
type messageBody struct {
	text        string
	replyToTS   string
	forwardFrom string
	forwarded   bool
}

func (b messageBody) metaFill(meta map[string]string) {
	if b.replyToTS != "" {
		meta["reply_to"] = b.replyToTS
	}
	if b.forwarded {
		meta["forwarded"] = "true"
		if b.forwardFrom != "" {
			meta["forwarded_from"] = b.forwardFrom
		}
	}
}

// messageBodyParse unwraps the reply, forward and file envelopes skychat
// carries in a message body (cmd/apps/skychat/commands reply.go, forward.go,
// group.go). It reports false for a delete tombstone, which is not a message.
func messageBodyParse(text string) (messageBody, bool) {
	t := strings.TrimSpace(text)
	if len(t) == 0 || t[0] != '{' || !strings.Contains(t, `"skychat_`) {
		return messageBody{text: text}, true
	}
	var env struct {
		Reply *struct {
			ToTS string `json:"to_ts"`
			Text string `json:"text"`
		} `json:"skychat_reply"`
		Forward *struct {
			From string `json:"from"`
			Text string `json:"text"`
		} `json:"skychat_forward"`
		File *struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"skychat_file"`
		Delete json.RawMessage `json:"skychat_delete"`
	}
	if err := json.Unmarshal([]byte(t), &env); err != nil {
		return messageBody{text: text}, true
	}
	switch {
	case env.Delete != nil:
		return messageBody{}, false
	case env.Reply != nil:
		b := messageBody{text: env.Reply.Text}
		if ts, err := time.Parse(time.RFC3339Nano, env.Reply.ToTS); err == nil {
			b.replyToTS = strconv.FormatInt(ts.UnixNano(), 10)
		}
		return b, true
	case env.Forward != nil:
		return messageBody{text: env.Forward.Text, forwarded: true, forwardFrom: env.Forward.From}, true
	case env.File != nil:
		return messageBody{text: fmt.Sprintf("[file] %s (%d bytes)", env.File.Name, env.File.Size)}, true
	}
	return messageBody{text: text}, true
}
