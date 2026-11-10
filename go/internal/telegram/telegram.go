// Package telegram is the small slice of the Telegram Bot API the operator's
// bot needs: reading updates by long polling, sending and editing messages with
// buttons, and answering button presses.
//
// The bot token is part of every request's address, and Go's HTTP errors quote
// the address. Nothing here lets the address out: errors name the method, never
// the URL, so the token cannot end up in a log.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultBase is the Bot API.
const DefaultBase = "https://api.telegram.org"

// Client talks to the Bot API as one bot.
type Client struct {
	token string
	Base  string
	http  *http.Client
}

// New returns a client for the bot with this token.
func New(token string) *Client {
	return &Client{token: token, Base: DefaultBase, http: &http.Client{Timeout: 90 * time.Second}}
}

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Name is how the account is shown to a person.
func (u User) Name() string {
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	if u.Username != "" {
		name += " (@" + u.Username + ")"
	}
	return name
}

// Chat is where a message is.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private" for a person's own chat with the bot
}

// Message is a message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text,omitempty"`
}

// CallbackQuery is a press of a button under a message.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Update is one thing that happened.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Button is a button under a message. Data comes back in the CallbackQuery
// when it is pressed; Telegram allows at most 64 bytes of it.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// Keyboard is rows of buttons.
type Keyboard [][]Button

// Command is an entry of the bot's menu.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// Error is the API saying no.
type Error struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int // seconds, when Telegram asks to slow down
}

func (e *Error) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// IsConflict reports whether another process is reading this bot's updates.
func IsConflict(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusConflict
}

// IsUnauthorized reports whether the token is not accepted.
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusUnauthorized
}

// IsNotModified reports an edit that would not change the message, which is
// harmless.
func IsNotModified(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusBadRequest &&
		bytes.Contains([]byte(e.Description), []byte("message is not modified"))
}

type envelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call posts params as JSON to a method and decodes the result into out.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: build the request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, scrub(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read the answer: %w", method, scrub(err))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &Error{Method: method, Code: resp.StatusCode, Description: "the answer is not JSON"}
	}
	if !env.OK {
		e := &Error{Method: method, Code: env.ErrorCode, Description: env.Description}
		if e.Code == 0 {
			e.Code = resp.StatusCode
		}
		if env.Parameters != nil {
			e.RetryAfter = env.Parameters.RetryAfter
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram %s: unexpected result: %w", method, err)
	}
	return nil
}

// scrub drops the request address from a transport error: it holds the token.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// GetMe returns the bot's own account; it is how a token is checked.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

// GetUpdates waits up to wait for updates from offset on. Passing the last
// update's id plus one tells Telegram everything before it has been handled.
func (c *Client) GetUpdates(ctx context.Context, offset int64, wait time.Duration) ([]Update, error) {
	var out []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(wait / time.Second),
		"allowed_updates": []string{"message", "callback_query"},
	}, &out)
	return out, err
}

type sendParams struct {
	ChatID      int64           `json:"chat_id"`
	MessageID   int64           `json:"message_id,omitempty"`
	Text        string          `json:"text"`
	ReplyMarkup *inlineKeyboard `json:"reply_markup,omitempty"`
	NoPreview   *linkPreview    `json:"link_preview_options,omitempty"`
}

type inlineKeyboard struct {
	InlineKeyboard Keyboard `json:"inline_keyboard"`
}

type linkPreview struct {
	IsDisabled bool `json:"is_disabled"`
}

func markup(kb Keyboard) *inlineKeyboard {
	if kb == nil {
		return nil
	}
	return &inlineKeyboard{InlineKeyboard: kb}
}

// Send sends plain text, with buttons if kb is not nil. There is no markup in
// the text on purpose: device names come from the devices themselves.
func (c *Client) Send(ctx context.Context, chatID int64, text string, kb Keyboard) (Message, error) {
	var m Message
	err := c.call(ctx, "sendMessage", sendParams{
		ChatID: chatID, Text: text, ReplyMarkup: markup(kb), NoPreview: &linkPreview{IsDisabled: true},
	}, &m)
	return m, err
}

// Edit replaces a message's text and buttons. A nil kb removes the buttons.
func (c *Client) Edit(ctx context.Context, chatID, messageID int64, text string, kb Keyboard) error {
	p := sendParams{ChatID: chatID, MessageID: messageID, Text: text, NoPreview: &linkPreview{IsDisabled: true}}
	if kb == nil {
		kb = Keyboard{}
	}
	p.ReplyMarkup = markup(kb)
	err := c.call(ctx, "editMessageText", p, nil)
	if IsNotModified(err) {
		return nil
	}
	return err
}

// Answer acknowledges a button press, with a short notice if text is set.
func (c *Client) Answer(ctx context.Context, callbackID, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, nil)
}

// SetCommands sets the bot's menu.
func (c *Client) SetCommands(ctx context.Context, cmds []Command) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}
