// Package bridge pkg/skychat/bridge/telegram_api.go c3-app-chat
// relays messages between a skychat group and other chat networks.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TelegramBaseURL is the Bot API endpoint.
const TelegramBaseURL = "https://api.telegram.org"

// TelegramAPI calls the Telegram Bot API over HTTPS.
// https://core.telegram.org/bots/api#making-requests
type TelegramAPI struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// TelegramError is an error answer from the Bot API.
type TelegramError struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *TelegramError) Error() string {
	return fmt.Sprintf("telegram: %d %s", e.Code, e.Description)
}

// TelegramUser is the sender of a message.
type TelegramUser struct {
	ID        int64  `json:"id"`
	Bot       bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// TelegramChat is the chat a message was posted in.
type TelegramChat struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// TelegramMessage holds the fields of a message the bridge relays.
type TelegramMessage struct {
	MessageID  int64            `json:"message_id"`
	From       *TelegramUser    `json:"from"`
	SenderChat *TelegramChat    `json:"sender_chat"`
	Chat       TelegramChat     `json:"chat"`
	Text       string           `json:"text"`
	Caption    string           `json:"caption"`
	ReplyTo    *TelegramMessage `json:"reply_to_message"`

	Photo     json.RawMessage `json:"photo"`
	Video     json.RawMessage `json:"video"`
	VideoNote json.RawMessage `json:"video_note"`
	Animation json.RawMessage `json:"animation"`
	Voice     json.RawMessage `json:"voice"`
	Audio     json.RawMessage `json:"audio"`
	Document  json.RawMessage `json:"document"`
	Location  json.RawMessage `json:"location"`
	Contact   json.RawMessage `json:"contact"`
	Poll      json.RawMessage `json:"poll"`
	// TopicCreated marks the root of a forum topic, which every message in
	// the topic replies to.
	TopicCreated json.RawMessage `json:"forum_topic_created"`
	Sticker      *struct {
		Emoji string `json:"emoji"`
	} `json:"sticker"`
}

// TelegramUpdate is one entry of a getUpdates answer.
type TelegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *TelegramMessage `json:"message"`
}

type telegramAnswer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// UpdatesFetch long polls getUpdates for new messages.
// https://core.telegram.org/bots/api#getupdates
func (a *TelegramAPI) UpdatesFetch(ctx context.Context, offset int64, timeout time.Duration) ([]TelegramUpdate, error) {
	params := map[string]any{
		"offset":          offset,
		"timeout":         int(timeout / time.Second),
		"allowed_updates": []string{"message"},
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	var updates []TelegramUpdate
	err := a.call(ctx, "getUpdates", params, &updates)
	return updates, err
}

// MessageSend posts plain text to a chat.
// https://core.telegram.org/bots/api#sendmessage
func (a *TelegramAPI) MessageSend(ctx context.Context, chatID int64, text string) error {
	params := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return a.call(ctx, "sendMessage", params, nil)
}

func (a *TelegramAPI) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	base := a.BaseURL
	if base == "" {
		base = TelegramBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+a.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return a.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return a.redact(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return a.redact(err)
	}
	var answer telegramAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return fmt.Errorf("telegram %s: http %d: %w", method, resp.StatusCode, err)
	}
	if !answer.OK {
		return &TelegramError{
			Code:        answer.ErrorCode,
			Description: answer.Description,
			RetryAfter:  time.Duration(answer.Parameters.RetryAfter) * time.Second,
		}
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(answer.Result, result)
}

// redact removes the token from an error, since net/http errors quote the
// request URL and the token is part of it.
func (a *TelegramAPI) redact(err error) error {
	if a.Token == "" || !strings.Contains(err.Error(), a.Token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), a.Token, "<token>"))
}
