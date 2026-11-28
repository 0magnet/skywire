// Package visor pkg/visor/hypervisor_handlers_ui2_chat.go c3-vis-core
package visor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/contacts"
	"github.com/skycoin/skywire/pkg/httputil"
	skychatgroup "github.com/skycoin/skywire/pkg/skychat/group"
	"github.com/skycoin/skywire/pkg/visor/ui2"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

// The chat tab. Direct messages go through the local skychat app as the
// Angular tab's /skychat/proxy calls do, rooms through the group RPCs.
// Like the Angular tab it serves only the hypervisor's own visor.

func (hv *Hypervisor) ui2ChatRoutes(r *httputil.Router) {
	r.Get("/visors/{pk}/chat", hv.ui2Chat(""))
	r.Get("/visors/{pk}/chat/log", hv.ui2Chat("log"))
	r.Get("/visors/{pk}/chat/avatar", hv.ui2ChatAvatar())
	r.Post("/visors/{pk}/chat/send", hv.ui2Chat("send"))
	r.Post("/visors/{pk}/chat/delete", hv.ui2Chat("delete"))
	r.Get("/visors/{pk}/chat/groups/log", hv.ui2Chat("glog"))
	r.Post("/visors/{pk}/chat/groups/send", hv.ui2Chat("gsend"))
	r.Post("/visors/{pk}/chat/groups/create", hv.ui2Chat("gcreate"))
	r.Post("/visors/{pk}/chat/groups/join", hv.ui2Chat("gjoin"))
	r.Get("/visors/{pk}/chat/groups/info", hv.ui2Chat("ginfo"))
	r.Get("/visors/{pk}/chat/groups/invite", hv.ui2Chat("ginvite"))
	r.Post("/visors/{pk}/chat/groups/add-member", hv.ui2Chat("gadd"))
	r.Post("/visors/{pk}/chat/groups/member", hv.ui2Chat("gmember"))
	r.Post("/visors/{pk}/chat/groups/request", hv.ui2Chat("grequest"))
	r.Post("/visors/{pk}/chat/groups/ask-again", hv.ui2Chat("gask"))
	r.Post("/visors/{pk}/chat/groups/leave", hv.ui2Chat("gleave"))
	r.Post("/visors/{pk}/chat/groups/delete", hv.ui2Chat("gdelete"))
	r.Post("/visors/{pk}/chat/groups/forget", hv.ui2Chat("gforget"))
	r.Post("/visors/{pk}/chat/forget", hv.ui2Chat("forget"))
	r.Post("/visors/{pk}/chat/forget-message", hv.ui2Chat("forgetmsg"))
	r.Get("/visors/{pk}/chat/search", hv.ui2Chat("search"))
	r.Post("/visors/{pk}/chat/privacy", hv.ui2Chat("privacy"))
	r.Post("/visors/{pk}/chat/profile", hv.ui2Chat("profile"))
	r.Post("/visors/{pk}/chat/contact", hv.ui2Chat("contact"))
	r.Post("/visors/{pk}/chat/groups/unsend", hv.ui2Chat("gunsend"))
	r.Post("/visors/{pk}/chat/groups/settings", hv.ui2Chat("gsettings"))
	r.Post("/visors/{pk}/chat/password", hv.ui2Chat("password"))
	r.Post("/visors/{pk}/chat/notify", hv.ui2Chat("notify"))
	hv.ui2VoiceRoutes(r)
}

// ui2ChatState is what the chat tab keeps between requests: the peers'
// profiles, fetched over the network at most once per ui2ChatProfileTTL, and
// the ids of the messages a read receipt was already sent for.
type ui2ChatState struct {
	mu       sync.Mutex
	profiles map[string]ui2ChatProfile
	read     map[string]bool
}

type ui2ChatProfile struct {
	p  visorapi.Profile
	ok bool
	at time.Time
}

const (
	ui2ChatProfileTTL = 30 * time.Minute
	// ui2ChatReadMax bounds the read receipt set, which restarts when full.
	ui2ChatReadMax = 5000
	// ui2ChatReadWindow is how old an inbound message may be and still get
	// a read receipt when its conversation is opened.
	ui2ChatReadWindow = 7 * 24 * time.Hour
)

// profileName is a cached profile name, without fetching.
func (s *ui2ChatState) profileName(pk string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.profiles[pk]; ok && e.ok {
		return e.p.Name
	}
	return ""
}

// profile is pk's profile, fetched when the cache has none or it is stale.
func (s *ui2ChatState) profile(api visorapi.API, pk cipher.PubKey) (visorapi.Profile, bool) {
	s.mu.Lock()
	e, ok := s.profiles[pk.Hex()]
	s.mu.Unlock()
	if ok && time.Since(e.at) < ui2ChatProfileTTL {
		return e.p, e.ok
	}
	p, err := api.ProfileFetch(pk)
	e = ui2ChatProfile{p: p, ok: err == nil, at: time.Now()}
	s.mu.Lock()
	if s.profiles == nil {
		s.profiles = map[string]ui2ChatProfile{}
	}
	s.profiles[pk.Hex()] = e
	s.mu.Unlock()
	return e.p, e.ok
}

// unread picks the ids of inbound messages not yet acknowledged and marks
// them as acknowledged.
func (s *ui2ChatState) unread(ms []ui2.ChatMsg) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.read == nil || len(s.read) > ui2ChatReadMax {
		s.read = map[string]bool{}
	}
	var ids []string
	for _, m := range ms {
		if m.Outgoing || m.ID == "" || m.Deleted || s.read[m.ID] || time.Since(m.TS) > ui2ChatReadWindow {
			continue
		}
		s.read[m.ID] = true
		ids = append(ids, m.ID)
	}
	return ids
}

// ui2ChatAvatar serves a peer's profile picture, or 404 when it has none.
func (hv *Hypervisor) ui2ChatAvatar() http.HandlerFunc {
	return hv.withCtx(hv.ui2VisorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		var pk cipher.PubKey
		if r.PathValue("pk") != hv.c.PK.Hex() || pk.Set(r.FormValue("pk")) != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=600")
		p, ok := hv.chat.profile(ctx.API, pk)
		mime, data, found := ui2ChatDataURI(p.Avatar)
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mime)
		_, _ = w.Write(data) //nolint:errcheck,gosec
	})
}

// ui2ChatDataURI decodes a base64 image data URI.
func ui2ChatDataURI(s string) (string, []byte, bool) {
	head, body, ok := strings.Cut(strings.TrimPrefix(s, "data:"), ",")
	if !ok || !strings.HasPrefix(s, "data:image/") || !strings.HasSuffix(head, ";base64") {
		return "", nil, false
	}
	b, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", nil, false
	}
	return strings.TrimSuffix(head, ";base64"), b, true
}

// ui2ChatNames are the names the contact book and the cached profiles give
// to keys. The contact book wins.
func (hv *Hypervisor) ui2ChatNames(api visorapi.API, pks []string) map[string]string {
	names := map[string]string{}
	for _, pk := range pks {
		if n := hv.chat.profileName(pk); n != "" {
			names[pk] = n
		}
	}
	if all, err := api.ContactsList(); err == nil {
		for _, c := range all {
			for _, k := range c.Keys {
				if c.Name != "" {
					names[k.Hex()] = c.Name
				}
			}
		}
	}
	return names
}

// ui2SkychatCall runs one request against the local skychat app.
func (hv *Hypervisor) ui2SkychatCall(method, path, query string, body any) ([]byte, int, error) {
	var rd *bytes.Reader
	hdr := http.Header{}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
		hdr.Set("Content-Type", "application/json")
	} else {
		rd = bytes.NewReader(nil)
	}
	code, _, out, err := hv.visor.SkychatProxy(method, path, query, hdr, rd)
	if err == nil && code/100 != 2 {
		err = errors.New(strings.TrimSpace(string(out)))
		if len(out) == 0 {
			err = errors.New(http.StatusText(code))
		}
	}
	return out, code, err
}

// ui2ChatDM fills the direct message view for peer.
func (hv *Hypervisor) ui2ChatDM(api visorapi.API, c ui2.Chat) ui2.Chat {
	c.History = true
	b, code, err := hv.ui2SkychatCall(http.MethodGet, "history", "limit=500", nil)
	var ms []ui2.ChatMsg
	switch {
	case code == http.StatusServiceUnavailable:
		c.History = false
	case err != nil:
		c.Err = err.Error()
	default:
		if ms, err = ui2.ParseChatHistory(b); err != nil {
			c.Err = err.Error()
		}
	}
	c = ui2.ChatOf(c, ms)
	c.Names = hv.ui2ChatNames(api, c.Peers)
	c.Notify = hv.ui2ChatNotify()
	c.Privacy = hv.ui2ChatPrivacy()
	return c
}

// ui2ChatRead sends the read receipts of the open conversation's inbound
// messages, as the Angular tab does for each message it shows.
func (hv *Hypervisor) ui2ChatRead(c ui2.Chat) {
	if c.Peer == "" {
		return
	}
	ids := hv.chat.unread(c.Msgs)
	if len(ids) == 0 {
		return
	}
	go func() {
		if _, _, err := hv.ui2SkychatCall(http.MethodPost, "read-receipt", "", map[string]any{"pk": c.Peer, "ids": ids}); err != nil {
			hv.logger.WithError(err).Debug("chat read receipt")
		}
	}()
}

// ui2ChatRooms fills the room view for the selected room.
func (hv *Hypervisor) ui2ChatRooms(api visorapi.API, c ui2.Chat, group string) ui2.Chat {
	c.Notify = hv.ui2ChatNotify()
	gs, err := api.GroupList()
	if err != nil {
		c.Err = err.Error()
		return c
	}
	c.Groups = gs
	for i := range gs {
		if gs[i].ID == group {
			c.Group = &gs[i]
		}
	}
	ms, err := api.GroupPoll(time.Time{})
	if err != nil {
		c.Err = err.Error()
		return c
	}
	c.AllGMsgs = ms
	var pks []string
	for _, m := range ms {
		pks = append(pks, m.SenderPK.Hex())
		if c.Group != nil && m.GroupID == group {
			c.GMsgs = append(c.GMsgs, m)
		}
	}
	if c.Group != nil {
		for _, m := range c.Group.Members {
			pks = append(pks, m.Hex())
		}
	}
	c.Names = hv.ui2ChatNames(api, pks)
	return c
}

// ui2ChatInfo fills the room panel: the rooms and, for its admins, the
// open join requests.
func (hv *Hypervisor) ui2ChatInfo(api visorapi.API, c ui2.Chat, group string) ui2.Chat {
	c = hv.ui2ChatRooms(api, c, group)
	if c.Names == nil {
		c.Names = map[string]string{}
	}
	if c.Moderator() {
		rs, err := api.GroupJoinRequests(group)
		if err != nil && c.Err == "" {
			c.Err = err.Error()
		}
		for _, q := range rs {
			if q.Status == skychatgroup.JoinStatusPending {
				c.Requests = append(c.Requests, q)
				if _, ok := c.Names[q.PK.Hex()]; !ok {
					if n := hv.chat.profileName(q.PK.Hex()); n != "" {
						c.Names[q.PK.Hex()] = n
					}
				}
			}
		}
	}
	return c
}

func (hv *Hypervisor) ui2Chat(op string) http.HandlerFunc {
	return hv.withCtx(hv.ui2VisorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		c := ui2.Chat{PK: r.PathValue("pk"), Mode: r.FormValue("mode"), Peer: r.FormValue("peer"), Compose: r.FormValue("new") != "", ShowEnded: r.FormValue("ended") != ""}
		if c.PK != hv.c.PK.Hex() || hv.visor == nil {
			c.Remote = true
			hv.render(w, r, ui2.ChatPage(c))
			return
		}
		var pk cipher.PubKey
		group := r.FormValue("group")
		switch op {
		case "":
			if c.Peer != "" && pk.Set(c.Peer) != nil {
				c.Peer = ""
			}
			c.Voice = ui2VoiceState(ctx.API, ui2.Voice{PK: c.PK, Peer: c.Peer, CanCall: c.Mode != "groups"})
			c.PwSet, _ = ctx.API.SkychatPasswordIsSet() //nolint:errcheck
			c.Privacy = hv.ui2ChatPrivacy()
			if p, err := ctx.API.ProfileGet(); err == nil {
				c.Profile = &p
			}
			if c.Mode == "groups" {
				hv.render(w, r, ui2.ChatPage(hv.ui2ChatRooms(ctx.API, c, group)))
				return
			}
			c = hv.ui2ChatDM(ctx.API, c)
			hv.ui2ChatRead(c)
			hv.render(w, r, ui2.ChatPage(c))
		case "log":
			c = hv.ui2ChatDM(ctx.API, c)
			hv.ui2ChatRead(c)
			hv.render(w, r, ui2.ChatLog(c), ui2.ChatPeers(c, true))
		case "send", "delete":
			var err error
			if op == "send" {
				c.Peer = strings.TrimSpace(r.FormValue("to"))
				if err = pk.Set(c.Peer); err == nil {
					network := r.FormValue("network")
					if network != "skynet" && network != "dmsg" {
						network = "auto"
					}
					body := map[string]any{"recipient": c.Peer, "message": r.FormValue("message"), "network": network, "receipts": true}
					if rt := r.FormValue("reply_to"); rt != "" {
						body["reply_to"] = rt
					}
					_, _, err = hv.ui2SkychatCall(http.MethodPost, "message", "", body)
				}
			} else {
				_, _, err = hv.ui2SkychatCall(http.MethodPost, "delete", "", map[string]string{"pk": c.Peer, "id": r.FormValue("id")})
			}
			if err == nil && op == "send" && r.FormValue("new") != "" {
				w.Header().Set("HX-Redirect", ui2.VisorURL(c.PK, "chat")+"?mode=dm&peer="+c.Peer)
				return
			}
			c = hv.ui2ChatDM(ctx.API, c)
			if err != nil {
				c.Err = err.Error()
			}
			hv.render(w, r, ui2.ChatLog(c), ui2.ChatPeers(c, true))
		case "glog":
			c.Mode = "groups"
			c = hv.ui2ChatRooms(ctx.API, c, group)
			hv.render(w, r, ui2.ChatRoomLog(c), ui2.ChatRooms(c, true))
		case "gsend":
			c.Mode = "groups"
			text, err := r.FormValue("text"), error(nil)
			if rt := r.FormValue("reply_to"); rt != "" {
				text, err = ui2.GroupReplyBody(text, rt, r.FormValue("reply_preview"))
			}
			if err == nil {
				err = ctx.API.GroupSend(visorapi.GroupSendArgs{ID: group, Text: text})
			}
			c = hv.ui2ChatRooms(ctx.API, c, group)
			if err != nil {
				hv.render(w, r, ui2.ChatRoomLog(c), ui2.Toast(err.Error(), true))
				return
			}
			hv.render(w, r, ui2.ChatRoomLog(c), ui2.ChatRooms(c, true))
		case "notify":
			hv.ui2ChatNotifySet(w, r, c)
		case "password":
			hv.render(w, r, ui2.ChatPassword(hv.ui2ChatPassword(r, ctx, c)))
		case "forgetmsg", "search", "privacy", "profile", "contact", "gunsend", "gsettings":
			hv.ui2ChatMore(w, r, ctx, c, op)
		case "forget":
			_, _, err := hv.ui2SkychatCall(http.MethodPost, "history/forget", "", map[string]any{"pk": c.Peer, "all": true})
			if err != nil {
				hv.render(w, r, ui2.Toast(err.Error(), true))
				return
			}
			w.Header().Set("HX-Redirect", ui2.VisorURL(c.PK, "chat")+"?mode=dm")
		case "gcreate", "gjoin", "gask", "gleave", "gdelete", "gforget":
			c.Mode = "groups"
			var err error
			var info visorapi.GroupInfo
			switch op {
			case "gcreate":
				mode := skychatgroup.ModePublic
				if r.FormValue("mode") == "private" {
					mode = skychatgroup.ModePrivate
				}
				info, _, err = ctx.API.GroupCreate(visorapi.GroupCreateArgs{Name: strings.TrimSpace(r.FormValue("name")), Mode: mode})
			case "gjoin":
				info, err = ctx.API.GroupJoin(visorapi.GroupJoinArgs{Invite: strings.TrimSpace(r.FormValue("invite"))})
			case "gask":
				info, err = ctx.API.GroupAskAgain(group)
			case "gleave":
				err = ctx.API.GroupLeave(group)
			case "gdelete":
				err = ctx.API.GroupDelete(group)
			case "gforget":
				err = ctx.API.GroupForget(group)
			}
			if err != nil {
				c = hv.ui2ChatRooms(ctx.API, c, group)
				hv.render(w, r, ui2.ChatRooms(c, false), ui2.Toast(err.Error(), true))
				return
			}
			loc := ui2.VisorURL(c.PK, "chat") + "?mode=groups"
			if info.ID != "" {
				loc += "&group=" + info.ID
			}
			w.Header().Set("HX-Redirect", loc)
		case "ginfo", "ginvite", "gadd", "gmember", "grequest":
			c.Mode = "groups"
			var err error
			var invite string
			if op != "ginfo" && op != "ginvite" {
				err = pk.Set(strings.TrimSpace(r.FormValue("pk")))
			}
			if err == nil {
				switch op {
				case "ginvite":
					invite, err = ctx.API.GroupInvite(group)
				case "gadd":
					_, err = ctx.API.GroupAddMember(group, pk)
				case "gmember":
					err = ui2ChatModerate(ctx.API, group, pk, r.FormValue("action"))
				case "grequest":
					if r.FormValue("action") == "approve" {
						_, err = ctx.API.GroupApproveJoin(group, pk)
					} else {
						err = ctx.API.GroupDenyJoin(group, pk)
					}
				}
			}
			c = hv.ui2ChatInfo(ctx.API, c, group)
			c.Invite = invite
			if err != nil {
				c.Err = err.Error()
			}
			hv.render(w, r, ui2.ChatRoomInfo(c))
		}
	})
}

// ui2ChatModerate runs one member action of the room panel.
func ui2ChatModerate(api visorapi.API, group string, pk cipher.PubKey, action string) error {
	var err error
	switch action {
	case "remove":
		_, err = api.GroupRemoveMember(group, pk)
	case "mute":
		_, err = api.GroupMuteMember(group, pk)
	case "unmute":
		_, err = api.GroupUnmuteMember(group, pk)
	case "promote":
		_, err = api.GroupPromoteAdmin(group, pk)
	case "demote":
		_, err = api.GroupDemoteAdmin(group, pk)
	default:
		err = errors.New("unknown action " + action)
	}
	return err
}

// ui2ChatPassword sets or clears the skychat password with the Angular
// panel's checks: 6 to 64 characters, typed twice, and the current one to
// change or clear a set password.
func (hv *Hypervisor) ui2ChatPassword(r *http.Request, ctx *httpCtx, c ui2.Chat) ui2.Chat {
	c.PwSet, _ = ctx.API.SkychatPasswordIsSet() //nolint:errcheck
	old, nw := r.FormValue("old"), r.FormValue("new")
	var err error
	switch {
	case r.FormValue("clear") != "":
		if old == "" {
			c.PwErr = hv.ui2T(r, "skychat.password.errors.old-required")
			return c
		}
		err = ctx.API.ClearSkychatPassword(old)
	case len(nw) < 6 || len(nw) > 64:
		c.PwErr = hv.ui2T(r, "skychat.password.errors.length")
		return c
	case nw != r.FormValue("confirm"):
		c.PwErr = hv.ui2T(r, "skychat.password.errors.mismatch")
		return c
	default:
		err = ctx.API.SetSkychatPassword(old, nw)
	}
	if err != nil {
		c.PwErr = err.Error()
		return c
	}
	c.PwSet, _ = ctx.API.SkychatPasswordIsSet() //nolint:errcheck
	return c
}

// ui2ChatNotify reads skychat's notification settings, nil on a skychat
// that has none.
func (hv *Hypervisor) ui2ChatNotify() *ui2.ChatNotify {
	b, _, err := hv.ui2SkychatCall(http.MethodGet, "notify-settings", "", nil)
	if err != nil {
		return nil
	}
	var n ui2.ChatNotify
	if json.Unmarshal(b, &n) != nil {
		return nil
	}
	return &n
}

// ui2ChatNotifySet turns notifications on or off, or mutes one chat, and
// answers with the button that changed.
func (hv *Hypervisor) ui2ChatNotifySet(w http.ResponseWriter, r *http.Request, c ui2.Chat) {
	key := r.FormValue("key")
	body := map[string]any{"key": key, "muted": r.FormValue("muted") == "1"}
	if key == "" {
		body = map[string]any{"enabled": r.FormValue("enabled") == "1"}
	}
	_, _, err := hv.ui2SkychatCall(http.MethodPost, "notify-settings", "", body)
	c.Notify = hv.ui2ChatNotify()
	out := ui2.ChatNotifyAll(c)
	if key != "" {
		out = ui2.ChatMute(c, key)
	}
	if err != nil {
		hv.render(w, r, out, ui2.Toast(err.Error(), true))
		return
	}
	hv.render(w, r, out)
}

// ui2ChatPrivacy reads skychat's /privacy, nil on a skychat without it.
func (hv *Hypervisor) ui2ChatPrivacy() *ui2.ChatPrivacy {
	b, _, err := hv.ui2SkychatCall(http.MethodGet, "privacy", "", nil)
	if err != nil {
		return nil
	}
	var p ui2.ChatPrivacy
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// ui2ChatMore runs the actions the skychat app's own page has: delete a
// message for this visor only, search, message requests and blocking, the
// profile name, adding a contact, deleting a room message for everyone and
// a room's settings.
func (hv *Hypervisor) ui2ChatMore(w http.ResponseWriter, r *http.Request, ctx *httpCtx, c ui2.Chat, op string) {
	group := r.FormValue("group")
	reload := func() {
		w.Header().Set("HX-Refresh", "true")
	}
	switch op {
	case "forgetmsg":
		_, _, err := hv.ui2SkychatCall(http.MethodPost, "history/forget", "", map[string]string{"pk": c.Peer, "id": r.FormValue("id")})
		c = hv.ui2ChatDM(ctx.API, c)
		if err != nil {
			c.Err = err.Error()
		}
		hv.render(w, r, ui2.ChatLog(c), ui2.ChatPeers(c, true))
	case "search":
		c.Query = strings.TrimSpace(r.FormValue("q"))
		if c.Query != "" {
			b, _, err := hv.ui2SkychatCall(http.MethodGet, "search", "q="+url.QueryEscape(c.Query), nil)
			if err == nil {
				_ = json.Unmarshal(b, &c.Hits) //nolint:errcheck
			}
			if gs, err := ctx.API.GroupList(); err == nil {
				c.Groups = gs
			}
			var pks []string
			for _, h := range c.Hits {
				pks = append(pks, h.Peer)
			}
			c.Names = hv.ui2ChatNames(ctx.API, pks)
		}
		hv.render(w, r, ui2.ChatHits(c))
	case "privacy":
		var path string
		var body map[string]any
		switch a := r.FormValue("action"); a {
		case "accept":
			path, body = "privacy/accept", map[string]any{"pk": r.FormValue("pk")}
		case "block", "unblock":
			path, body = "privacy/block", map[string]any{"pk": r.FormValue("pk"), "blocked": a == "block"}
		case "approval-on", "approval-off":
			path, body = "privacy", map[string]any{"approval": a == "approval-on"}
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		_, _, err := hv.ui2SkychatCall(http.MethodPost, path, "", body)
		if err != nil {
			hv.render(w, r, ui2.Toast(err.Error(), true))
			return
		}
		if r.FormValue("peer") != "" {
			reload()
			return
		}
		hv.ui2ChatSettingsRender(w, r, ctx, c)
	case "profile":
		p, err := ctx.API.ProfileGet()
		if err == nil {
			args := visorapi.ProfileSetArgs{Name: strings.TrimSpace(r.FormValue("name")), Avatar: p.Avatar}
			if r.FormValue("clear_avatar") != "" {
				args.Avatar = ""
			}
			_, err = ctx.API.ProfileSet(args)
		}
		if err != nil {
			hv.render(w, r, ui2.Toast(err.Error(), true))
			return
		}
		hv.ui2ChatSettingsRender(w, r, ctx, c)
	case "contact":
		var pk cipher.PubKey
		name := strings.TrimSpace(r.Header.Get("HX-Prompt"))
		err := pk.Set(r.FormValue("peer"))
		if err == nil && name == "" {
			err = errors.New(hv.ui2T(r, "ui2.chat.add-contact-prompt"))
		}
		if err == nil {
			_, err = ctx.API.ContactsPut(contacts.Contact{Name: name, Keys: []cipher.PubKey{pk}})
		}
		if err != nil {
			hv.render(w, r, ui2.Toast(err.Error(), true))
			return
		}
		reload()
	case "gunsend":
		c.Mode = "groups"
		ts, err := strconv.ParseInt(r.FormValue("ts"), 10, 64)
		if err == nil {
			var tomb string
			if tomb, err = ui2.GroupDeleteBody(ts); err == nil {
				err = ctx.API.GroupSend(visorapi.GroupSendArgs{ID: group, Text: tomb})
			}
			if err == nil {
				if uerr := ctx.API.GroupUnsend(visorapi.GroupUnsendArgs{ID: group, TS: ts}); uerr != nil {
					hv.logger.WithError(uerr).Debug("chat unsend: the tombstone still hides it")
				}
			}
		}
		c = hv.ui2ChatRooms(ctx.API, c, group)
		if err != nil {
			hv.render(w, r, ui2.ChatRoomLog(c), ui2.Toast(err.Error(), true))
			return
		}
		hv.render(w, r, ui2.ChatRoomLog(c))
	case "gsettings":
		c.Mode = "groups"
		on := r.FormValue("on") == "1"
		var err error
		switch r.FormValue("action") {
		case "rename":
			_, err = ctx.API.GroupSetMeta(visorapi.GroupSetMetaArgs{ID: group, Name: strings.TrimSpace(r.FormValue("name"))})
		case "readonly":
			_, err = ctx.API.GroupSetReadOnly(group, on)
		case "listed":
			_, err = ctx.API.GroupSetListed(group, on)
		case "rotate":
			_, err = ctx.API.GroupRotateKey(group)
		default:
			err = errors.New("unknown action")
		}
		c = hv.ui2ChatInfo(ctx.API, c, group)
		if err != nil {
			c.Err = err.Error()
		}
		hv.render(w, r, ui2.ChatRoomInfo(c))
	}
}

func (hv *Hypervisor) ui2ChatSettingsRender(w http.ResponseWriter, r *http.Request, ctx *httpCtx, c ui2.Chat) {
	c.Privacy = hv.ui2ChatPrivacy()
	if p, err := ctx.API.ProfileGet(); err == nil {
		c.Profile = &p
	}
	if c.Privacy != nil {
		c.Names = hv.ui2ChatNames(ctx.API, c.Privacy.Blocked)
	}
	hv.render(w, r, ui2.ChatSettings(c))
}
