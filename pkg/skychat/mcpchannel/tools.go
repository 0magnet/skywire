// Package mcpchannel pkg/skychat/mcpchannel/tools.go c3-app-chat
package mcpchannel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/skycoin/skywire/pkg/cipher"
	skychatgroup "github.com/skycoin/skywire/pkg/skychat/group"
	"github.com/skycoin/skywire/pkg/visor/visorapi"
)

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	Failed  bool          `json:"isError,omitempty"`
}

var toolDefinitions = []map[string]any{
	{
		"name":        "reply",
		"description": "Send a skychat message to a group or DM. Pass chat_id from the inbound <channel> tag.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"chat_id":  map[string]any{"type": "string", "description": "group id, or the peer public key for a DM"},
				"text":     map[string]any{"type": "string"},
				"reply_to": map[string]any{"type": "string", "description": "message_id to quote; only when answering an earlier message"},
			},
			"required": []string{"chat_id", "text"},
		},
	},
	{
		"name":        "history",
		"description": "Recent messages of a group or DM, oldest first, one per line.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"chat_id": map[string]any{"type": "string"},
				"limit":   map[string]any{"type": "integer", "description": "default 20, at most 100"},
			},
			"required": []string{"chat_id"},
		},
	},
	{
		"name":        "groups",
		"description": "Groups this visor is an active member of: id, member count, name.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

const (
	historyLimitDefault = 20
	historyLimitMax     = 100
	replyPreviewMax     = 80
)

// chatTarget is a parsed chat_id: a group id or a DM peer.
type chatTarget struct {
	groupID string
	peer    cipher.PubKey
}

func chatTargetParse(chatID string) (chatTarget, error) {
	chatID = strings.TrimSpace(chatID)
	if _, err := uuid.Parse(chatID); err == nil {
		return chatTarget{groupID: chatID}, nil
	}
	var pk cipher.PubKey
	if err := pk.UnmarshalText([]byte(chatID)); err == nil && !pk.Null() {
		return chatTarget{peer: pk}, nil
	}
	return chatTarget{}, fmt.Errorf("chat_id %q is neither a group id nor a public key", chatID)
}

func (s *Server) toolCall(_ context.Context, name string, raw json.RawMessage) (string, error) {
	var args struct {
		ChatID  string `json:"chat_id"`
		Text    string `json:"text"`
		ReplyTo string `json:"reply_to"`
		Limit   int    `json:"limit"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	switch name {
	case "reply":
		return s.replySend(args.ChatID, args.Text, args.ReplyTo)
	case "history":
		return s.historyRead(args.ChatID, args.Limit)
	case "groups":
		return s.groupsList()
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func (s *Server) replySend(chatID, text, replyTo string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("text is empty")
	}
	target, err := chatTargetParse(chatID)
	if err != nil {
		return "", err
	}
	if s.conf.ReplyPrefix {
		text = BotPrefix + text
	}
	if target.groupID != "" {
		body, err := s.groupReplyBody(target.groupID, text, replyTo)
		if err != nil {
			return "", err
		}
		s.inbound.sentRecord(target.groupID, body)
		err = s.groupLink.call(func(v Visor) error {
			return v.GroupSend(visorapi.GroupSendArgs{ID: target.groupID, Text: body})
		})
		if err != nil {
			return "", err
		}
		return "sent", nil
	}
	payload := map[string]any{"recipient": target.peer.Hex(), "message": text, "network": "skynet"}
	if replyTo != "" {
		payload["reply_to"] = replyTo
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	err = s.groupLink.call(func(v Visor) error {
		out, err := v.SkychatHTTP(visorapi.SkychatHTTPIn{
			Method:  "POST",
			Path:    "message",
			Headers: map[string][]string{"Content-Type": {"application/json"}},
			Body:    body,
		})
		if err != nil {
			return err
		}
		if out.Status < 200 || out.Status > 299 {
			return fmt.Errorf("skychat /message: HTTP %d: %s", out.Status, strings.TrimSpace(string(out.Body)))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sent", nil
}

// groupReplyBody wraps text in skychat's reply envelope when replyTo names a
// message (cmd/apps/skychat/commands/reply.go replyMeta).
func (s *Server) groupReplyBody(groupID, text, replyTo string) (string, error) {
	if replyTo == "" {
		return text, nil
	}
	nanos, err := strconv.ParseInt(replyTo, 10, 64)
	if err != nil {
		return "", fmt.Errorf("reply_to %q is not a group message_id", replyTo)
	}
	reply := map[string]string{
		"to_ts": time.Unix(0, nanos).UTC().Format(time.RFC3339Nano),
		"text":  text,
	}
	if ref, ok := s.inbound.recentGet(groupID + "/" + replyTo); ok {
		reply["to_sender"] = ref.sender
		preview := []rune(ref.text)
		if len(preview) > replyPreviewMax {
			preview = preview[:replyPreviewMax]
		}
		reply["to_preview"] = string(preview)
	}
	b, err := json.Marshal(map[string]any{"skychat_reply": reply})
	return string(b), err
}

func (s *Server) historyRead(chatID string, limit int) (string, error) {
	target, err := chatTargetParse(chatID)
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = historyLimitDefault
	}
	limit = min(limit, historyLimitMax)
	var b strings.Builder
	if target.groupID != "" {
		var msgs []visorapi.GroupMessage
		err := s.groupLink.call(func(v Visor) error {
			var err error
			msgs, err = v.GroupHistory(target.groupID, limit)
			return err
		})
		if err != nil {
			return "", err
		}
		for _, m := range msgs {
			body, ok := messageBodyParse(m.Text)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "%s %d %s: %s\n", m.TS.UTC().Format(time.RFC3339), m.TS.UnixNano(), m.SenderPK.Hex(), oneLine(body.text))
		}
		return historyText(b.String()), nil
	}
	var msgs []struct {
		Outgoing  bool      `json:"outgoing"`
		Text      string    `json:"text"`
		Timestamp time.Time `json:"timestamp"`
	}
	err = s.groupLink.call(func(v Visor) error {
		out, err := v.SkychatHTTP(visorapi.SkychatHTTPIn{
			Method: "GET",
			Path:   "history",
			Query:  "limit=" + strconv.Itoa(limit) + "&peer=" + url.QueryEscape(target.peer.Hex()),
		})
		if err != nil {
			return err
		}
		if out.Status != 200 {
			return fmt.Errorf("skychat /history: HTTP %d: %s", out.Status, strings.TrimSpace(string(out.Body)))
		}
		return json.Unmarshal(out.Body, &msgs)
	})
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		body, ok := messageBodyParse(m.Text)
		if !ok {
			continue
		}
		dir := "in"
		if m.Outgoing {
			dir = "out"
		}
		fmt.Fprintf(&b, "%s %s: %s\n", m.Timestamp.UTC().Format(time.RFC3339), dir, oneLine(body.text))
	}
	return historyText(b.String()), nil
}

func historyText(s string) string {
	if s == "" {
		return "no messages"
	}
	return s
}

func oneLine(s string) string {
	return strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s)
}

func (s *Server) groupsList() (string, error) {
	var list []visorapi.GroupInfo
	err := s.groupLink.call(func(v Visor) error {
		var err error
		list, err = v.GroupList()
		return err
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, g := range list {
		if g.Status != skychatgroup.StatusActive {
			continue
		}
		fmt.Fprintf(&b, "%s %d %s\n", g.ID, len(g.Members), g.Name)
	}
	if b.Len() == 0 {
		return "no groups", nil
	}
	return b.String(), nil
}
