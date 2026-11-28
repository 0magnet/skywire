// Package ui2 pkg/visor/ui2/chat.go c3-vis-core
package ui2

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

// The chat tab, as skychat.component.ts: direct messages read from
// skychat's history and sent through its /message endpoint, and the
// visor's group rooms. Laid out after Telegram Web: a chat list on the
// left, the open thread on the right.

// ChatMsg is one direct message, as skychat's /history serves it
// (pkg/skychat/history.Message).
type ChatMsg struct {
	Peer     string    `json:"peer"`
	From     string    `json:"from"`
	Outgoing bool      `json:"outgoing"`
	Text     string    `json:"text"`
	Message  string    `json:"message"`
	TS       time.Time `json:"timestamp"`
	ID       string    `json:"id"`
	ReplyTo  string    `json:"reply_to"`
	Deleted  bool      `json:"deleted"`

	FileName       string `json:"file_name"`
	FileSize       int64  `json:"file_size"`
	FileStatus     string `json:"file_status"`
	FileURL        string `json:"file_url"`
	ReplyToPreview string `json:"reply_to_preview"`
	Forwarded      bool   `json:"forwarded"`
	ForwardFrom    string `json:"forward_from"`
}

// Body is the text, from whichever field skychat filled.
func (m ChatMsg) Body() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Message
}

// Chat is the chat tab.
type Chat struct {
	PK     string
	Remote bool
	Mode   string // "dm" or "groups"
	Err    string

	// Names maps a public key to its contact book or profile name.
	Names map[string]string

	// Direct messages.
	History bool
	Peers   []string
	Peer    string
	// Compose is the new message view, where the recipient is typed.
	Compose bool
	Msgs    []ChatMsg
	all     []ChatMsg
	Draft   string
	ReplyTo string

	// PwSet is whether the skychat password gates the standalone page.
	PwSet bool
	PwErr string

	Voice Voice

	// Groups. AllGMsgs holds every room's messages, for the list previews.
	Groups   []visorapi.GroupInfo
	Group    *visorapi.GroupInfo
	GMsgs    []visorapi.GroupMessage
	AllGMsgs []visorapi.GroupMessage
	Invite   string

	// Notify is the skychat notification settings, nil on a skychat without them.
	Notify *ChatNotify

	// ShowEnded lists the rooms this visor left or deleted.
	ShowEnded bool

	// Privacy is who may message this visor, nil on a skychat without it.
	Privacy *ChatPrivacy
	// Profile is this visor's own skychat profile, nil when grouping is off.
	Profile *visorapi.Profile
	// Query and Hits are a message search.
	Query string
	Hits  []ChatHit

	// Requests are the open join requests of Group, for its admins.
	Requests []visorapi.GroupJoinRequest
}

// ParseChatHistory reads skychat's /history and sorts it oldest first.
func ParseChatHistory(b []byte) ([]ChatMsg, error) {
	var ms []ChatMsg
	if err := json.Unmarshal(b, &ms); err != nil {
		return nil, err
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].TS.Before(ms[j].TS) })
	return ms, nil
}

// ChatOf picks the peers, newest conversation first, and peer's messages.
func ChatOf(c Chat, ms []ChatMsg) Chat {
	seen := map[string]bool{}
	for i := len(ms) - 1; i >= 0; i-- {
		p := ms[i].Peer
		if p != "" && !seen[p] {
			seen[p] = true
			c.Peers = append(c.Peers, p)
		}
	}
	if c.Peer != "" && !seen[c.Peer] {
		c.Peers = append([]string{c.Peer}, c.Peers...)
	}
	for _, m := range ms {
		if m.Peer == c.Peer {
			c.Msgs = append(c.Msgs, m)
		}
	}
	c.all = ms
	return c
}

// QuoteOf is the start of the message a reply points to, or "".
func (c Chat) QuoteOf(id string) string {
	for _, m := range c.Msgs {
		if m.ID == id {
			return chatClip(m.Body(), 80)
		}
	}
	return ""
}

func chatClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	b := []rune(s)
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return s
}

func chatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("15:04")
}

// chatListTime is the time of a list row: the clock today, else the date.
func chatListTime(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t, now = t.Local(), now.Local()
	switch {
	case sameDay(t, now):
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("01-02")
	default:
		return t.Format("2006-01-02")
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// chatDay is the separator above the first message of a day.
func chatDay(ctx context.Context, t, now time.Time) string {
	t, now = t.Local(), now.Local()
	switch {
	case sameDay(t, now):
		return T(ctx, "ui2.chat.today")
	case sameDay(t, now.AddDate(0, 0, -1)):
		return T(ctx, "ui2.chat.yesterday")
	default:
		return t.Format("2006-01-02")
	}
}

func (c Chat) url(rest string) string { return VisorURL(c.PK, "chat") + rest }

// ThreadOpen is whether a phone shows the thread rather than the list.
func (c Chat) ThreadOpen() bool {
	if c.Mode == "groups" {
		return c.Group != nil
	}
	return c.Peer != "" || c.Compose
}

// chatGroupTerm is a group mode or role (public, private, owner, member) in
// the page language, or v itself when it has no translation.
func chatGroupTerm(ctx context.Context, v string) string {
	key := "ui2.chat." + v
	if s := T(ctx, key); s != key {
		return s
	}
	return v
}

// NameOf is the name of a key from the contact book or its profile, or "".
func (c Chat) NameOf(pk string) string { return c.Names[pk] }

// chatInitials are the letters on an avatar: the first letters of the first
// two words of a name, or two characters of a key after its 02/03 prefix.
func chatInitials(name, id string) string {
	var out []rune
	for _, w := range strings.Fields(name) {
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				out = append(out, unicode.ToUpper(r))
				break
			}
		}
		if len(out) == 2 {
			break
		}
	}
	if len(out) > 0 {
		return string(out)
	}
	if len(id) >= 4 {
		return strings.ToUpper(id[2:4])
	}
	return strings.ToUpper(id)
}

// chatHue picks one of the seven avatar colors from an id, as Telegram does.
func chatHue(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id)) //nolint:errcheck
	return "av" + strconv.Itoa(int(h.Sum32()%7))
}

// ChatRow is one conversation in the list.
type ChatRow struct {
	ID       string
	Href     string
	Name     string // empty shows ID, which the page's labels may replace
	Mono     bool
	Avatar   string
	Preview  string
	Mine     bool // the preview is a message this visor sent
	Time     time.Time
	Inbound  []int64 // unix ms of the latest inbound messages, for the unread badge
	Active   bool
	Group    *visorapi.GroupInfo
	Disabled string // a room's pending or declined state
}

// chatInboundMax caps how many inbound times a row carries for its badge.
const chatInboundMax = 99

func (r ChatRow) inboundAttr() string {
	s := make([]string, len(r.Inbound))
	for i, t := range r.Inbound {
		s[i] = strconv.FormatInt(t, 10)
	}
	return strings.Join(s, " ")
}

func (r ChatRow) searchAttr() string {
	return strings.ToLower(r.Name + " " + r.ID + " " + r.Preview)
}

// avatarURL is the route that serves a peer's profile picture.
func (c Chat) avatarURL(pk string) string { return c.url("/avatar?pk=" + pk) }

// PeerRows are the direct conversations, newest first.
func (c Chat) PeerRows() []ChatRow {
	byPeer := map[string]*ChatRow{}
	rows := make([]ChatRow, 0, len(c.Peers))
	for _, p := range c.Peers {
		rows = append(rows, ChatRow{
			ID: p, Href: c.url("?mode=dm&peer=" + p), Name: c.NameOf(p), Mono: c.NameOf(p) == "",
			Avatar: c.avatarURL(p), Active: p == c.Peer,
		})
	}
	for i := range rows {
		byPeer[rows[i].ID] = &rows[i]
	}
	for _, m := range c.all {
		r := byPeer[m.Peer]
		if r == nil {
			continue
		}
		r.Time, r.Mine, r.Preview = m.TS, m.Outgoing, chatPreview(m)
		if !m.Outgoing && !m.TS.IsZero() {
			r.Inbound = append(r.Inbound, m.TS.UnixMilli())
			if len(r.Inbound) > chatInboundMax {
				r.Inbound = r.Inbound[1:]
			}
		}
	}
	return rows
}

func chatPreview(m ChatMsg) string {
	switch {
	case m.Deleted:
		return ""
	case m.FileName != "" && m.Body() == "":
		return "📎 " + m.FileName
	}
	return chatClip(m.Body(), 60)
}

// RoomRows are the rooms, the ones with the newest message first.
func (c Chat) RoomRows() []ChatRow {
	rows := make([]ChatRow, 0, len(c.Groups))
	idx := map[string]int{}
	for i := range c.Groups {
		g := &c.Groups[i]
		if ChatGroupEnded(string(g.Status)) && !c.ShowEnded && (c.Group == nil || c.Group.ID != g.ID) {
			continue
		}
		r := ChatRow{ID: g.ID, Href: c.url("?mode=groups&group=" + g.ID + c.endedQuery()), Name: g.Name, Group: g,
			Active: c.Group != nil && c.Group.ID == g.ID, Time: g.LastMessageAt}
		if strings.HasPrefix(g.Avatar, "data:image/") {
			r.Avatar = g.Avatar
		}
		switch string(g.Status) {
		case "awaiting_approval", "denied":
			r.Disabled = string(g.Status)
		}
		idx[g.ID] = len(rows)
		rows = append(rows, r)
	}
	for _, m := range c.AllGMsgs {
		if _, tomb := parseGroupDelete(m.Text); tomb {
			continue
		}
		i, ok := idx[m.GroupID]
		if !ok {
			continue
		}
		r := &rows[i]
		mine := m.SenderPK.Hex() == c.PK
		if !m.TS.Before(r.Time) || r.Preview == "" {
			r.Time, r.Mine = m.TS, mine
			r.Preview = chatClip(groupText(m.Text), 60)
			if n := c.NameOf(m.SenderPK.Hex()); n != "" && !mine {
				r.Preview = n + ": " + r.Preview
			}
		}
		if !mine && !m.TS.IsZero() {
			r.Inbound = append(r.Inbound, m.TS.UnixMilli())
			if len(r.Inbound) > chatInboundMax {
				r.Inbound = r.Inbound[1:]
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Time.Equal(rows[j].Time) {
			return rows[i].Time.After(rows[j].Time)
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

// ChatLine is one bubble of the thread, for both direct messages and rooms.
type ChatLine struct {
	ID        string
	From      string
	Out       bool
	Text      string
	TS        time.Time
	ReplyTo   string
	Quote     string
	Deleted   bool
	Forwarded bool
	File      ChatFile
	// TSNano names a group message for delete for everyone.
	TSNano int64

	Day   string // set on the first line of a day
	First bool   // first of a run from one sender
	Last  bool   // last of that run
}

// ChatFile is a file a message carries.
type ChatFile struct {
	Name   string
	Size   int64
	Status string
	URL    string // the download through the hypervisor's skychat proxy
	Thumb  string // a thumbnail, for an image
}

// chatRunGap ends a run of bubbles from one sender, as Telegram groups them.
const chatRunGap = 10 * time.Minute

// Lines is the open thread as bubbles, with day separators and runs.
func (c Chat) Lines(ctx context.Context, now time.Time) []ChatLine {
	var ls []ChatLine
	if c.Mode == "groups" {
		gone := map[string]bool{}
		for _, m := range c.GMsgs {
			if d, ok := parseGroupDelete(m.Text); ok {
				gone[m.SenderPK.Hex()+"|"+strconv.FormatInt(d.ToTSNano, 10)] = true
			}
		}
		for _, m := range c.GMsgs {
			if _, tomb := parseGroupDelete(m.Text); tomb || gone[m.SenderPK.Hex()+"|"+strconv.FormatInt(m.TS.UnixNano(), 10)] {
				continue
			}
			l := ChatLine{ID: GroupMsgID(m.SenderPK.Hex(), m.TS.Format(time.RFC3339Nano)), From: m.SenderPK.Hex(), Out: m.SenderPK.Hex() == c.PK, Text: m.Text, TS: m.TS, TSNano: m.TS.UnixNano()}
			switch {
			case isGroupReply(m.Text):
				r, _ := ParseGroupReply(m.Text)
				l.Text, l.Quote = r.Text, chatClip(r.ToPreview, 80)
				l.ReplyTo = GroupMsgID(r.ToSender, r.ToTS)
			case isGroupFile(m.Text):
				f, _ := parseGroupFile(m.Text)
				l.Text, l.File = "", ChatFile{Name: f.Name, Size: f.Size}
			case isGroupForward(m.Text):
				f, _ := parseGroupForward(m.Text)
				l.Text, l.Forwarded = f.Text, true
			}
			ls = append(ls, l)
		}
	} else {
		for _, m := range c.Msgs {
			l := ChatLine{ID: m.ID, From: m.Peer, Out: m.Outgoing, Text: m.Body(), TS: m.TS, ReplyTo: m.ReplyTo,
				Deleted: m.Deleted, Forwarded: m.Forwarded}
			if m.ReplyTo != "" {
				l.Quote = c.QuoteOf(m.ReplyTo)
				if l.Quote == "" {
					l.Quote = chatClip(m.ReplyToPreview, 80)
				}
			}
			if m.FileName != "" {
				l.File = c.fileOf(m)
			}
			ls = append(ls, l)
		}
	}
	for i := range ls {
		l := &ls[i]
		if i == 0 || !sameDay(ls[i-1].TS.Local(), l.TS.Local()) {
			l.Day = chatDay(ctx, l.TS, now)
		}
		l.First = i == 0 || l.Day != "" || ls[i-1].Out != l.Out || ls[i-1].From != l.From || l.TS.Sub(ls[i-1].TS) > chatRunGap
		if i > 0 && l.First {
			ls[i-1].Last = true
		}
	}
	if len(ls) > 0 {
		ls[len(ls)-1].Last = true
	}
	return ls
}

var chatImageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}

func (c Chat) fileOf(m ChatMsg) ChatFile {
	f := ChatFile{Name: m.FileName, Size: m.FileSize, Status: m.FileStatus}
	if name := strings.TrimPrefix(m.FileURL, "/files/"); name != m.FileURL && name != "" && !strings.Contains(name, "/") {
		proxy := "api/visors/" + c.PK + "/skychat/proxy/"
		f.URL = proxy + "files/" + name
		if chatImageExt[strings.ToLower(path.Ext(name))] {
			f.Thumb = proxy + "thumb/" + name
		}
	}
	return f
}

// Title is the open thread's name.
func (c Chat) Title() string {
	if c.Group != nil {
		return c.Group.Name
	}
	return c.NameOf(c.Peer)
}

// MemberRows are the members of the open room, owner and admins first.
func (c Chat) MemberRows() []ChatMember {
	if c.Group == nil {
		return nil
	}
	g := c.Group
	in := func(list []string, pk string) bool {
		for _, x := range list {
			if x == pk {
				return true
			}
		}
		return false
	}
	var admins, muted []string
	for _, a := range g.Admins {
		admins = append(admins, a.Hex())
	}
	for _, a := range g.Muted {
		muted = append(muted, a.Hex())
	}
	out := make([]ChatMember, 0, len(g.Members))
	for _, m := range g.Members {
		pk := m.Hex()
		r := ChatMember{PK: pk, Name: c.NameOf(pk), Muted: in(muted, pk), Self: pk == c.PK}
		switch {
		case m == g.OwnerPK:
			r.Role = "owner"
		case in(admins, pk):
			r.Role = "admin"
		}
		out = append(out, r)
	}
	rank := map[string]int{"owner": 0, "admin": 1, "": 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Role] < rank[out[j].Role] })
	return out
}

// ChatMember is one member of a room.
type ChatMember struct {
	PK    string
	Name  string
	Role  string // owner, admin or ""
	Muted bool
	Self  bool
}

// Moderator is whether this visor may manage the open room's members.
func (c Chat) Moderator() bool {
	if c.Group == nil {
		return false
	}
	if c.Group.Role == "owner" {
		return true
	}
	for _, a := range c.Group.Admins {
		if a.Hex() == c.PK {
			return true
		}
	}
	return false
}

// Owner is whether this visor owns the open room.
func (c Chat) Owner() bool { return c.Group != nil && c.Group.Role == "owner" }

func chatMode(c Chat) string {
	if c.Mode == "groups" {
		return "groups"
	}
	return "dm"
}

// chatOpenID is the open conversation's id, whose messages count as read.
func chatOpenID(c Chat) string {
	if c.Group != nil {
		return c.Group.ID
	}
	return c.Peer
}

func chatGroupAvatar(c Chat) string {
	if c.Group != nil && strings.HasPrefix(c.Group.Avatar, "data:image/") {
		return c.Group.Avatar
	}
	return ""
}

// chatMemberVerb is the action a member button posts: mute or unmute,
// promote or demote, remove.
func chatMemberVerb(action string, on bool) string {
	switch action {
	case "mute":
		if on {
			return "unmute"
		}
		return "mute"
	case "admin":
		if on {
			return "demote"
		}
		return "promote"
	}
	return action
}

func chatMemberIcon(action string, on bool) string {
	switch action {
	case "mute":
		if on {
			return "volume_up"
		}
		return "volume_off"
	case "admin":
		if on {
			return "remove_moderator"
		}
		return "add_moderator"
	}
	return "person_remove"
}

func chatMemberConfirm(ctx context.Context, action string) string {
	if action == "remove" {
		return T(ctx, "ui2.chat.remove-confirm")
	}
	return ""
}

func chatPwTitle(ctx context.Context, set bool) string {
	if set {
		return T(ctx, "skychat.password.change")
	}
	return T(ctx, "skychat.password.set")
}

// ChatNotify is skychat's notification settings (/notify-settings).
type ChatNotify struct {
	Enabled bool     `json:"enabled"`
	Muted   []string `json:"muted"`
}

// MutedKey is whether the conversation key is muted.
func (n *ChatNotify) MutedKey(key string) bool {
	if n == nil {
		return false
	}
	for _, k := range n.Muted {
		if k == key {
			return true
		}
	}
	return false
}

// GroupReply is a quoted reply in a group message body, the
// {"skychat_reply":...} envelope of cmd/apps/skychat/commands/reply.go.
type GroupReply struct {
	ToSender  string `json:"to_sender,omitempty"`
	ToTS      string `json:"to_ts,omitempty"`
	ToPreview string `json:"to_preview,omitempty"`
	Text      string `json:"text"`
}

type groupReplyEnvelope struct {
	Reply *GroupReply `json:"skychat_reply"`
}

// ParseGroupReply reads a reply envelope from a group message body.
func ParseGroupReply(text string) (GroupReply, bool) {
	t := strings.TrimSpace(text)
	if t == "" || t[0] != '{' || !strings.Contains(t, "skychat_reply") {
		return GroupReply{}, false
	}
	var env groupReplyEnvelope
	if err := json.Unmarshal([]byte(t), &env); err != nil || env.Reply == nil {
		return GroupReply{}, false
	}
	return *env.Reply, true
}

// GroupReplyBody is the body that sends text as a reply to the message
// GroupMsgID names, quoting preview.
func GroupReplyBody(text, msgID, preview string) (string, error) {
	sender, ts, ok := strings.Cut(strings.TrimPrefix(msgID, "g|"), "|")
	if !ok || !strings.HasPrefix(msgID, "g|") {
		return "", errors.New("bad reply reference")
	}
	b, err := json.Marshal(groupReplyEnvelope{Reply: &GroupReply{ToSender: sender, ToTS: ts, ToPreview: chatClip(preview, 120), Text: text}})
	return string(b), err
}

// GroupMsgID names a group message by its sender and RFC 3339 time, as the
// reply envelope locates its parent.
func GroupMsgID(sender, ts string) string { return "g|" + sender + "|" + ts }

func groupText(s string) string {
	if r, ok := ParseGroupReply(s); ok {
		return r.Text
	}
	if f, ok := parseGroupFile(s); ok {
		return "📎 " + f.Name
	}
	if f, ok := parseGroupForward(s); ok {
		return f.Text
	}
	return s
}

// ChatGroupEnded is whether a room's status means this visor is out of it
// for good: left, deleted, banned or removed. Such a room can be forgotten.
func ChatGroupEnded(status string) bool {
	switch status {
	case "left", "revoked", "banned", "removed":
		return true
	}
	return false
}

// EndedCount is how many rooms the list hides as ended.
func (c Chat) EndedCount() int {
	n := 0
	for _, g := range c.Groups {
		if ChatGroupEnded(string(g.Status)) {
			n++
		}
	}
	return n
}

func (c Chat) endedQuery() string {
	if c.ShowEnded {
		return "&ended=1"
	}
	return ""
}

// The group message envelopes of cmd/apps/skychat/commands (group.go,
// forward.go): a file reference, a forward and a delete tombstone, which
// hides an earlier message by the same author.
type groupFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type groupForward struct {
	From string `json:"from,omitempty"`
	Text string `json:"text"`
}

type groupDelete struct {
	ToTSNano int64 `json:"to_ts_nano"`
}

type groupEnvelope struct {
	File    *groupFile    `json:"skychat_file,omitempty"`
	Forward *groupForward `json:"skychat_forward,omitempty"`
	Delete  *groupDelete  `json:"skychat_delete,omitempty"`
}

func parseGroupEnvelope(text, key string) (groupEnvelope, bool) {
	t := strings.TrimSpace(text)
	if t == "" || t[0] != '{' || !strings.Contains(t, key) {
		return groupEnvelope{}, false
	}
	var env groupEnvelope
	return env, json.Unmarshal([]byte(t), &env) == nil
}

func parseGroupFile(text string) (groupFile, bool) {
	env, ok := parseGroupEnvelope(text, "skychat_file")
	if !ok || env.File == nil || env.File.ID == "" {
		return groupFile{}, false
	}
	return *env.File, true
}

func parseGroupForward(text string) (groupForward, bool) {
	env, ok := parseGroupEnvelope(text, "skychat_forward")
	if !ok || env.Forward == nil {
		return groupForward{}, false
	}
	return *env.Forward, true
}

func parseGroupDelete(text string) (groupDelete, bool) {
	env, ok := parseGroupEnvelope(text, "skychat_delete")
	if !ok || env.Delete == nil || env.Delete.ToTSNano == 0 {
		return groupDelete{}, false
	}
	return *env.Delete, true
}

func isGroupReply(s string) bool   { _, ok := ParseGroupReply(s); return ok }
func isGroupFile(s string) bool    { _, ok := parseGroupFile(s); return ok }
func isGroupForward(s string) bool { _, ok := parseGroupForward(s); return ok }

// GroupDeleteBody is the tombstone that hides this visor's message tsNano
// for everyone in the group.
func GroupDeleteBody(tsNano int64) (string, error) {
	b, err := json.Marshal(groupEnvelope{Delete: &groupDelete{ToTSNano: tsNano}})
	return string(b), err
}

// ChatPrivacy is who may message this visor, skychat's /privacy.
type ChatPrivacy struct {
	Approval bool     `json:"approval"`
	Blocked  []string `json:"blocked"`
	Requests []string `json:"requests"`
}

// Requesting is whether pk's messages wait to be accepted or blocked.
func (p *ChatPrivacy) Requesting(pk string) bool { return p != nil && slices.Contains(p.Requests, pk) }

// BlockedKey is whether pk is blocked.
func (p *ChatPrivacy) BlockedKey(pk string) bool { return p != nil && slices.Contains(p.Blocked, pk) }

// ChatHit is one message skychat's /search found.
type ChatHit struct {
	Peer     string    `json:"peer,omitempty"`
	GroupID  string    `json:"group_id,omitempty"`
	From     string    `json:"from,omitempty"`
	Outgoing bool      `json:"outgoing,omitempty"`
	Text     string    `json:"text"`
	TS       time.Time `json:"ts"`
}

// HitHref opens the conversation a hit belongs to.
func (c Chat) HitHref(h ChatHit) string {
	if h.GroupID != "" {
		return c.url("?mode=groups&ended=1&group=" + h.GroupID)
	}
	return c.url("?mode=dm&peer=" + h.Peer)
}

// HitTitle names the conversation a hit belongs to.
func (c Chat) HitTitle(h ChatHit) string {
	if h.GroupID != "" {
		for _, g := range c.Groups {
			if g.ID == h.GroupID {
				return g.Name
			}
		}
		return h.GroupID
	}
	if n := c.NameOf(h.Peer); n != "" {
		return n
	}
	return h.Peer
}

// b01Approval is the privacy action that flips approval.
func b01Approval(on bool) string {
	if on {
		return "approval-off"
	}
	return "approval-on"
}
