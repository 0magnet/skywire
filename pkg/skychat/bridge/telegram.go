// Package bridge pkg/skychat/bridge/telegram.go c3-app-chat
package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/skycoin/skywire/pkg/logging"
)

// Prefixes mark which network a relayed line came from. The skychat side
// also uses TelegramPrefix to recognize the bridge's own posts.
const (
	TelegramPrefix = "[TG] "
	SkychatPrefix  = "[skychat] "
)

// telegramTextLimit is the sendMessage text limit.
// https://core.telegram.org/bots/api#sendmessage
const telegramTextLimit = 4096

const (
	seenLimit       = 1024
	sentEchoLimit   = 256
	profileCacheTTL = time.Hour
	profileMissTTL  = 10 * time.Minute
	backoffMin      = time.Second
	backoffMax      = time.Minute
)

// GroupMessage is one message from the skychat group.
type GroupMessage struct {
	TimestampNs int64
	SenderPK    string
	Body        string
}

// SkychatGroup is the bridge's view of one skychat group on a visor.
type SkychatGroup interface {
	// Send posts text to the group as the visor.
	Send(ctx context.Context, text string) error
	// Stream delivers group messages newer than sinceNs, then live ones,
	// until ctx ends or the connection fails. sinceNs 0 means live only.
	Stream(ctx context.Context, sinceNs int64, deliver func(GroupMessage)) error
	// SelfPK is the hex public key of the visor the bridge posts as.
	SelfPK(ctx context.Context) (string, error)
	// ProfileName is the profile name a member publishes, or "".
	ProfileName(ctx context.Context, pk string) (string, error)
}

// TelegramConfig configures a Telegram bridge.
type TelegramConfig struct {
	API       *TelegramAPI
	ChatID    int64
	Group     SkychatGroup
	StatePath string
	Log       *logging.Logger

	// TelegramInterval spaces messages to Telegram, which allows about
	// 20 a minute in a group. Lines waiting meanwhile are joined.
	TelegramInterval time.Duration
	// SkychatInterval spaces messages to the skychat group.
	SkychatInterval time.Duration
	// PollTimeout is the getUpdates long poll timeout.
	PollTimeout time.Duration
	// QueueLimit caps the lines waiting in each direction. Past it the
	// oldest lines are dropped and logged.
	QueueLimit int
}

// Telegram relays between one Telegram chat and one skychat group.
type Telegram struct {
	conf TelegramConfig
	log  *logging.Logger

	mu       sync.Mutex
	state    State
	seen     map[string]struct{}
	sentEcho []string
	selfPK   string
	profiles map[string]profileEntry

	// Drop counters let a delivery loop tell how many of the lines it sent
	// are still at the head of the queue after an overflow.
	telegramDropped int
	skychatDropped  int

	telegramWake chan struct{}
	skychatWake  chan struct{}
}

type profileEntry struct {
	name    string
	expires time.Time
}

// TelegramNew loads the state file and prepares a bridge.
func TelegramNew(conf TelegramConfig) (*Telegram, error) {
	if conf.API == nil || conf.Group == nil || conf.StatePath == "" || conf.ChatID == 0 {
		return nil, errors.New("bridge: API, Group, StatePath and ChatID are required")
	}
	if conf.TelegramInterval <= 0 {
		conf.TelegramInterval = 3 * time.Second
	}
	if conf.SkychatInterval <= 0 {
		conf.SkychatInterval = time.Second
	}
	if conf.PollTimeout <= 0 {
		conf.PollTimeout = 50 * time.Second
	}
	if conf.QueueLimit <= 0 {
		conf.QueueLimit = 1000
	}
	if conf.Log == nil {
		conf.Log = logging.MustGetLogger("telegram_bridge")
	}
	st, err := StateLoad(conf.StatePath)
	if err != nil {
		return nil, fmt.Errorf("bridge: load state: %w", err)
	}
	b := &Telegram{
		conf:         conf,
		log:          conf.Log,
		state:        st,
		seen:         make(map[string]struct{}, len(st.SkychatSeen)),
		profiles:     make(map[string]profileEntry),
		telegramWake: make(chan struct{}, 1),
		skychatWake:  make(chan struct{}, 1),
	}
	for _, k := range st.SkychatSeen {
		b.seen[k] = struct{}{}
	}
	return b, nil
}

// Run relays until ctx ends.
func (b *Telegram) Run(ctx context.Context) {
	b.log.Infof("bridging telegram chat %d, resuming at offset %d and group timestamp %d with %d+%d lines queued",
		b.conf.ChatID, b.state.TelegramOffset, b.state.SkychatSinceNs, len(b.state.ToTelegram), len(b.state.ToSkychat))
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){b.telegramPoll, b.telegramDeliver, b.skychatStream, b.skychatDeliver} {
		wg.Add(1)
		go func(loop func(context.Context)) {
			defer wg.Done()
			loop(ctx)
		}(loop)
	}
	wg.Wait()
}

// State returns a copy of the current state.
func (b *Telegram) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state
	s.ToTelegram = append([]QueuedLine(nil), s.ToTelegram...)
	s.ToSkychat = append([]QueuedLine(nil), s.ToSkychat...)
	s.SkychatSeen = append([]string(nil), s.SkychatSeen...)
	return s
}

// saveLocked writes the state file. The caller holds mu.
func (b *Telegram) saveLocked() {
	if err := StateSave(b.conf.StatePath, b.state); err != nil {
		b.log.Errorf("save state: %v", err)
	}
}

func (b *Telegram) telegramPoll(ctx context.Context) {
	backoff := backoffMin
	for ctx.Err() == nil {
		b.mu.Lock()
		offset := b.state.TelegramOffset
		b.mu.Unlock()
		updates, err := b.conf.API.UpdatesFetch(ctx, offset, b.conf.PollTimeout)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			wait := backoff
			var te *TelegramError
			if errors.As(err, &te) && te.RetryAfter > 0 {
				wait = te.RetryAfter
			}
			b.log.Warnf("telegram getUpdates: %v (retrying in %s)", err, wait)
			if !sleep(ctx, wait) {
				return
			}
			backoff = backoffNext(backoff)
			continue
		}
		backoff = backoffMin
		if len(updates) == 0 {
			continue
		}
		b.mu.Lock()
		for _, u := range updates {
			if u.UpdateID >= b.state.TelegramOffset {
				b.state.TelegramOffset = u.UpdateID + 1
			}
			if u.Message == nil || u.Message.Chat.ID != b.conf.ChatID {
				continue
			}
			text, ok := TelegramFormat(u.Message)
			if !ok {
				continue
			}
			b.state.ToSkychat = b.enqueueLocked(b.state.ToSkychat, QueuedLine{Text: text}, "skychat", &b.skychatDropped)
		}
		b.saveLocked()
		b.mu.Unlock()
		wake(b.skychatWake)
	}
}

func (b *Telegram) skychatDeliver(ctx context.Context) {
	backoff := backoffMin
	for {
		b.mu.Lock()
		lines := b.state.ToSkychat
		droppedBefore := b.skychatDropped
		var parts []string
		for _, l := range lines {
			parts = append(parts, l.Text)
		}
		b.mu.Unlock()
		if len(parts) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-b.skychatWake:
				continue
			}
		}
		text, n := coalesce(parts, telegramTextLimit)
		if err := b.conf.Group.Send(ctx, text); err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Warnf("skychat group send: %v (retrying in %s)", err, backoff)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = backoffNext(backoff)
			continue
		}
		backoff = backoffMin
		b.mu.Lock()
		b.state.ToSkychat = b.state.ToSkychat[sentHead(n, b.skychatDropped-droppedBefore, len(b.state.ToSkychat)):]
		b.sentEcho = append(b.sentEcho, text)
		if len(b.sentEcho) > sentEchoLimit {
			b.sentEcho = b.sentEcho[len(b.sentEcho)-sentEchoLimit:]
		}
		b.saveLocked()
		b.mu.Unlock()
		b.log.Infof("relayed %d telegram line(s) to the skychat group", n)
		if !sleep(ctx, b.conf.SkychatInterval) {
			return
		}
	}
}

func (b *Telegram) skychatStream(ctx context.Context) {
	backoff := backoffMin
	for ctx.Err() == nil {
		self, err := b.conf.Group.SelfPK(ctx)
		if err == nil {
			b.mu.Lock()
			b.selfPK = self
			since := b.state.SkychatSinceNs
			b.mu.Unlock()
			err = b.conf.Group.Stream(ctx, since, func(m GroupMessage) {
				backoff = backoffMin
				b.skychatAccept(m)
			})
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stream closed")
		}
		b.log.Warnf("skychat group stream: %v (reconnecting in %s)", err, backoff)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = backoffNext(backoff)
	}
}

// skychatAccept queues one group message for Telegram unless it was seen
// before or is the bridge's own post.
func (b *Telegram) skychatAccept(m GroupMessage) {
	key := fmt.Sprintf("%d:%s", m.TimestampNs, m.SenderPK)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[key]; ok {
		return
	}
	b.seen[key] = struct{}{}
	b.state.SkychatSeen = append(b.state.SkychatSeen, key)
	if len(b.state.SkychatSeen) > seenLimit {
		for _, old := range b.state.SkychatSeen[:len(b.state.SkychatSeen)-seenLimit] {
			delete(b.seen, old)
		}
		b.state.SkychatSeen = append([]string(nil), b.state.SkychatSeen[len(b.state.SkychatSeen)-seenLimit:]...)
	}
	if m.TimestampNs > b.state.SkychatSinceNs {
		b.state.SkychatSinceNs = m.TimestampNs
	}
	if b.echoLocked(m) {
		b.saveLocked()
		return
	}
	b.log.Infof("queued skychat message ts=%d from %s for telegram", m.TimestampNs, m.SenderPK)
	b.state.ToTelegram = b.enqueueLocked(b.state.ToTelegram, QueuedLine{SenderPK: m.SenderPK, Text: m.Body}, "telegram", &b.telegramDropped)
	b.saveLocked()
	wake(b.telegramWake)
}

// echoLocked reports whether m is a post the bridge made itself.
func (b *Telegram) echoLocked(m GroupMessage) bool {
	if m.SenderPK != b.selfPK || b.selfPK == "" {
		return false
	}
	if strings.HasPrefix(m.Body, TelegramPrefix) {
		return true
	}
	for _, s := range b.sentEcho {
		if s == m.Body {
			return true
		}
	}
	return false
}

func (b *Telegram) telegramDeliver(ctx context.Context) {
	backoff := backoffMin
	for {
		b.mu.Lock()
		lines := append([]QueuedLine(nil), b.state.ToTelegram...)
		droppedBefore := b.telegramDropped
		b.mu.Unlock()
		if len(lines) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-b.telegramWake:
				continue
			}
		}
		parts := make([]string, 0, len(lines))
		for _, l := range lines {
			parts = append(parts, SkychatPrefix+b.displayName(ctx, l.SenderPK)+": "+l.Text)
			if len(strings.Join(parts, "\n")) >= telegramTextLimit {
				break
			}
		}
		text, n := coalesce(parts, telegramTextLimit)
		err := b.conf.API.MessageSend(ctx, b.conf.ChatID, text)
		if ctx.Err() != nil {
			return
		}
		var te *TelegramError
		switch {
		case err == nil:
			backoff = backoffMin
			b.log.Infof("relayed %d skychat line(s) to telegram", n)
		case errors.As(err, &te) && te.RetryAfter > 0:
			b.log.Warnf("telegram sendMessage rate limited, waiting %s", te.RetryAfter)
			if !sleep(ctx, te.RetryAfter) {
				return
			}
			continue
		case errors.As(err, &te) && te.Code == 400:
			// The API refused this text and will refuse it again.
			b.log.Errorf("telegram sendMessage refused %d line(s), dropping them: %v", n, err)
		default:
			b.log.Warnf("telegram sendMessage: %v (retrying in %s)", err, backoff)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = backoffNext(backoff)
			continue
		}
		b.mu.Lock()
		b.state.ToTelegram = b.state.ToTelegram[sentHead(n, b.telegramDropped-droppedBefore, len(b.state.ToTelegram)):]
		b.saveLocked()
		b.mu.Unlock()
		if !sleep(ctx, b.conf.TelegramInterval) {
			return
		}
	}
}

// enqueueLocked appends a line, dropping the oldest past QueueLimit.
func (b *Telegram) enqueueLocked(queue []QueuedLine, line QueuedLine, to string, dropped *int) []QueuedLine {
	queue = append(queue, line)
	if over := len(queue) - b.conf.QueueLimit; over > 0 {
		b.log.Errorf("queue to %s is over %d lines, dropped the %d oldest", to, b.conf.QueueLimit, over)
		*dropped += over
		queue = append([]QueuedLine(nil), queue[over:]...)
	}
	return queue
}

// displayName is the sender's profile name, or a short key when it has
// none. Lookups are cached so a busy group does not ask on every line.
func (b *Telegram) displayName(ctx context.Context, pk string) string {
	now := time.Now()
	b.mu.Lock()
	e, ok := b.profiles[pk]
	b.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.name
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name, err := b.conf.Group.ProfileName(lookupCtx, pk)
	ttl := profileCacheTTL
	if err != nil || strings.TrimSpace(name) == "" {
		name, ttl = PKShort(pk), profileMissTTL
	}
	name = strings.TrimSpace(name)
	b.mu.Lock()
	b.profiles[pk] = profileEntry{name: name, expires: now.Add(ttl)}
	b.mu.Unlock()
	return name
}

// PKShort is a display label for a key, the first 8 and last 4 hex digits
// as the skychat UIs show it. Logs carry the full key.
func PKShort(pk string) string {
	if len(pk) <= 12 {
		return pk
	}
	return pk[:8] + "…" + pk[len(pk)-4:]
}

// TelegramFormat renders a Telegram message as a skychat line. It returns
// false for messages that are not relayed, such as bot commands.
func TelegramFormat(m *TelegramMessage) (string, bool) {
	if strings.HasPrefix(m.Text, "/") {
		return "", false
	}
	body := m.Text
	if body == "" {
		kind := telegramMediaKind(m)
		if kind == "" {
			return "", false
		}
		body = strings.TrimSpace("[" + kind + "] " + m.Caption)
	}
	return TelegramPrefix + TelegramName(m) + telegramReplyQuote(m.ReplyTo) + ": " + body, true
}

// TelegramName is the sender's first and last name, or @username.
func TelegramName(m *TelegramMessage) string {
	if m.SenderChat != nil && m.SenderChat.Title != "" {
		return m.SenderChat.Title
	}
	if m.From == nil {
		return "unknown"
	}
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" && m.From.Username != "" {
		name = "@" + m.From.Username
	}
	if name == "" {
		name = fmt.Sprintf("user %d", m.From.ID)
	}
	return name
}

func telegramMediaKind(m *TelegramMessage) string {
	switch {
	case len(m.Photo) > 0:
		return "photo"
	case len(m.Video) > 0, len(m.VideoNote) > 0:
		return "video"
	case len(m.Animation) > 0:
		return "gif"
	case len(m.Voice) > 0:
		return "voice"
	case len(m.Audio) > 0:
		return "audio"
	case len(m.Document) > 0:
		return "file"
	case m.Sticker != nil:
		return strings.TrimSpace("sticker " + m.Sticker.Emoji)
	case len(m.Location) > 0:
		return "location"
	case len(m.Contact) > 0:
		return "contact"
	case len(m.Poll) > 0:
		return "poll"
	}
	return ""
}

// coalesce joins leading parts with newlines while the result stays within
// limit runes, and returns how many parts it used. A first part longer
// than limit is cut.
func coalesce(parts []string, limit int) (string, int) {
	text := truncateRunes(parts[0], limit)
	n := 1
	for ; n < len(parts); n++ {
		next := text + "\n" + parts[n]
		if utf8.RuneCountInString(next) > limit {
			break
		}
		text = next
	}
	return text, n
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit-1]) + "…"
}

// sentHead is how many lines to remove from the head of a queue after n were
// delivered and dropped of them fell off the head meanwhile.
func sentHead(n, dropped, length int) int {
	n -= dropped
	if n < 0 {
		n = 0
	}
	if n > length {
		n = length
	}
	return n
}

func backoffNext(d time.Duration) time.Duration {
	d *= 2
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// telegramReplyQuote is " (reply to name: start of text)" for a reply, or
// "". A reply to a relayed skychat line names the skychat sender.
func telegramReplyQuote(r *TelegramMessage) string {
	if r == nil || len(r.TopicCreated) > 0 {
		return ""
	}
	quoted := r.Text
	if quoted == "" {
		quoted = r.Caption
	}
	if quoted == "" {
		kind := telegramMediaKind(r)
		if kind == "" {
			return ""
		}
		quoted = "[" + kind + "]"
	}
	name := TelegramName(r)
	if r.From != nil && r.From.Bot && strings.HasPrefix(quoted, SkychatPrefix) {
		if sender, text, ok := strings.Cut(strings.TrimPrefix(quoted, SkychatPrefix), ": "); ok {
			name, quoted = sender, text
		}
	}
	quoted = strings.Join(strings.Fields(quoted), " ")
	return " (reply to " + name + ": " + truncateRunes(quoted, 40) + ")"
}
