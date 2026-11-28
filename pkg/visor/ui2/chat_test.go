package ui2

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

func TestChatOf(t *testing.T) {
	ms, err := ParseChatHistory([]byte(`[
		{"peer":"b","outgoing":true,"text":"two","timestamp":"2026-11-11T10:00:02Z","id":"2"},
		{"peer":"a","outgoing":false,"message":"one","timestamp":"2026-11-11T10:00:01Z","id":"1"},
		{"peer":"a","outgoing":true,"text":"re","timestamp":"2026-11-11T10:00:03Z","id":"3","reply_to":"1"}]`))
	require.NoError(t, err)
	c := ChatOf(Chat{Peer: "a"}, ms)
	require.Equal(t, []string{"a", "b"}, c.Peers, "newest conversation first")
	require.Len(t, c.Msgs, 2)
	require.Equal(t, "one", c.Msgs[0].Body())
	require.Equal(t, "one", c.QuoteOf("1"))
	c = ChatOf(Chat{Peer: "new"}, ms)
	require.Equal(t, "new", c.Peers[0], "a peer picked before any message is listed")
}

func TestChatLines(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.Local)
	c := Chat{Peer: "a", Msgs: []ChatMsg{
		{Peer: "a", Text: "one", TS: now.AddDate(0, 0, -1), ID: "1"},
		{Peer: "a", Text: "two", TS: now.Add(-time.Hour), ID: "2"},
		{Peer: "a", Text: "three", TS: now.Add(-time.Hour + time.Minute), ID: "3"},
		{Peer: "a", Outgoing: true, Text: "four", TS: now, ID: "4", ReplyTo: "2"},
	}}
	ls := c.Lines(renderCtx(), now)
	require.Len(t, ls, 4)
	require.Equal(t, "Yesterday", ls[0].Day)
	require.Equal(t, "Today", ls[1].Day)
	require.Empty(t, ls[2].Day)
	require.True(t, ls[1].First)
	require.False(t, ls[1].Last, "a run continues within ten minutes")
	require.False(t, ls[2].First)
	require.True(t, ls[2].Last, "the sender changes after it")
	require.True(t, ls[3].First && ls[3].Last && ls[3].Out)
	require.Equal(t, "two", ls[3].Quote)
}

func TestChatRows(t *testing.T) {
	ms, err := ParseChatHistory([]byte(`[
		{"peer":"02aa","outgoing":false,"text":"hello","timestamp":"2026-11-11T10:00:01Z","id":"1"},
		{"peer":"02aa","outgoing":true,"text":"hi","timestamp":"2026-11-11T10:00:02Z","id":"2"},
		{"peer":"03bb","outgoing":false,"file_name":"a.png","file_url":"/files/a.png","timestamp":"2026-11-11T09:00:00Z","id":"3"}]`))
	require.NoError(t, err)
	c := ChatOf(Chat{PK: "self", Names: map[string]string{"02aa": "Alice Test"}}, ms)
	rows := c.PeerRows()
	require.Len(t, rows, 2)
	require.Equal(t, "Alice Test", rows[0].Name)
	require.Equal(t, "hi", rows[0].Preview)
	require.True(t, rows[0].Mine)
	require.Len(t, rows[0].Inbound, 1)
	require.Equal(t, "📎 a.png", rows[1].Preview)
	c.Peer = "03bb"
	c = ChatOf(c, ms)
	f := c.Lines(renderCtx(), time.Now())[0].File
	require.Equal(t, "api/visors/self/skychat/proxy/files/a.png", f.URL)
	require.Equal(t, "api/visors/self/skychat/proxy/thumb/a.png", f.Thumb)
	require.Equal(t, "AT", chatInitials("Alice Test", "02aa"))
	require.Equal(t, "BB", chatInitials("", "03bbcc"))
}

func renderCtx() context.Context {
	return With(context.Background(), Page{Base: "./", Lang: "en"}, NewCatalog(os.DirFS("../static")))
}

func TestChatRender(t *testing.T) {
	pk, _ := cipher.GenerateKeyPair()
	peer, _ := cipher.GenerateKeyPair()
	g := visorapi.GroupInfo{ID: "g1", Name: "Test room", Mode: "private", Role: "owner", Status: "active", OwnerPK: pk,
		Members: []cipher.PubKey{pk, peer}, Muted: []cipher.PubKey{peer}, CanPost: true}
	c := Chat{PK: pk.Hex(), Mode: "groups", Groups: []visorapi.GroupInfo{g}, Voice: Voice{Off: true}}
	c.Group = &c.Groups[0]
	c.GMsgs = []visorapi.GroupMessage{{GroupID: "g1", SenderPK: peer, Text: "hello room", TS: time.Now()}}
	c.AllGMsgs = c.GMsgs
	out := renderEN(t, ChatPage(c))
	require.Contains(t, out, "hello room")
	require.Contains(t, out, `class="tg thread-open"`)
	info := renderEN(t, ChatRoomInfo(c))
	require.Contains(t, info, "Delete room")
	require.Contains(t, info, `title="Unmute"`)
	require.Contains(t, info, "Make admin")

	c = Chat{PK: pk.Hex(), Peer: peer.Hex(), Voice: Voice{Off: true}, History: true}
	c = ChatOf(c, []ChatMsg{{Peer: peer.Hex(), Outgoing: true, Text: "hi", ID: "1", TS: time.Now()}})
	out = renderEN(t, ChatPage(c))
	require.Contains(t, out, "data-tick")
	require.Contains(t, out, `name="network"`)
}
