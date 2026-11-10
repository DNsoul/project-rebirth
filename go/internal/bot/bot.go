// Package bot is the operator's Telegram bot: it tells them when a device asks
// to come in, lets them approve, refuse and cut off devices, shows how the
// server is doing, and passes on the maintenance they ask for.
//
// It answers its owners only, and only in their own chats with it; everybody
// else gets no answer at all, not even a refusal. It runs inside the manager
// of the server the apps are using, and on no other: when the server hands
// over, the replacement's manager takes the bot over.
//
// What it sends goes through Telegram's servers and stays there, and chats with
// a bot are not end-to-end encrypted. So it never sends an address, neither the
// server's nor anybody's on the devices: devices are shown by the name their
// app gives (the model) and a four-digit code.
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/telegram"
)

// API is the part of the Bot API used here.
type API interface {
	GetUpdates(ctx context.Context, offset int64, wait time.Duration) ([]telegram.Update, error)
	Send(ctx context.Context, chatID int64, text string, kb telegram.Keyboard) (telegram.Message, error)
	Edit(ctx context.Context, chatID, messageID int64, text string, kb telegram.Keyboard) error
	Answer(ctx context.Context, callbackID, text string) error
	SetCommands(ctx context.Context, cmds []telegram.Command) error
}

// Devices is the node's control socket.
type Devices interface {
	Control(ctx context.Context, req node.ControlRequest) (node.ControlResponse, error)
}

// Manager is the server's manager.
type Manager interface {
	Snapshot() lifecycle.Snapshot
	Do(a lifecycle.Action) error
}

// SocketDevices reaches the node through its control socket.
type SocketDevices struct{ Path string }

// Control implements Devices.
func (s SocketDevices) Control(ctx context.Context, req node.ControlRequest) (node.ControlResponse, error) {
	path := s.Path
	if path == "" {
		path = node.DefaultControlPath
	}
	return node.Control(ctx, path, req)
}

// Bot is the operator's bot.
type Bot struct {
	API     API
	Devices Devices
	Manager Manager
	Owners  []int64

	Log *slog.Logger
	Now func() time.Time

	// Timings, shortened in tests.
	Wait       time.Duration // how long one long poll waits for updates
	Idle       time.Duration // the pause while not the server that answers
	WatchEvery time.Duration // how often new requests are looked for

	events chan lifecycle.Event

	mu          sync.Mutex
	ctx         context.Context
	offset      int64
	commandsSet bool
}

// Defaults for the timings.
const (
	DefaultWait       = 50 * time.Second
	DefaultIdle       = 5 * time.Second
	DefaultWatchEvery = 5 * time.Second
)

func (b *Bot) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Bot) log() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (b *Bot) init() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.events == nil {
		b.events = make(chan lifecycle.Event, 32)
	}
	if b.Wait == 0 {
		b.Wait = DefaultWait
	}
	if b.Idle == 0 {
		b.Idle = DefaultIdle
	}
	if b.WatchEvery == 0 {
		b.WatchEvery = DefaultWatchEvery
	}
}

// Notify hands the bot an event from the manager. It never blocks: an event
// that does not fit is dropped, since the operator can always ask.
func (b *Bot) Notify(e lifecycle.Event) {
	b.init()
	select {
	case b.events <- e:
	default:
		b.log().Warn("dropped an event for the operator: too many waiting", "kind", e.Kind)
	}
}

// Run answers the operator until the context ends.
func (b *Bot) Run(ctx context.Context) error {
	b.init()
	b.mu.Lock()
	b.ctx = ctx
	b.mu.Unlock()

	go b.watch(ctx)

	for ctx.Err() == nil {
		snap := b.Manager.Snapshot()
		if !snap.Current {
			sleep(ctx, b.Idle)
			continue
		}
		if !b.commandsSet {
			if err := b.API.SetCommands(ctx, menuCommands); err == nil {
				b.commandsSet = true
			}
		}
		updates, err := b.API.GetUpdates(ctx, b.offset, b.Wait)
		switch {
		case ctx.Err() != nil:
			return nil
		case telegram.IsConflict(err):
			// Another server is reading: the one just handed over to, or the
			// one this one replaces, for the minute both are up. Whichever is
			// published keeps the bot; this looks again soon.
			b.log().Debug("another process reads the bot's updates", "error", err)
			sleep(ctx, 2*b.Idle)
			continue
		case telegram.IsUnauthorized(err):
			b.log().Error("Telegram does not accept the bot token; the bot is silent until it is replaced")
			sleep(ctx, 60*b.Idle)
			continue
		case err != nil:
			b.log().Warn("could not read the bot's updates", "error", err)
			sleep(ctx, b.Idle)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
			b.handle(ctx, u)
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// watch tells the owners what they should hear without asking: events from
// the manager, and devices that have just asked to come in.
func (b *Bot) watch(ctx context.Context) {
	t := time.NewTicker(b.WatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-b.events:
			snap := b.Manager.Snapshot()
			// A server that has handed over says so once more, and one that
			// has just taken over says so before its snapshot shows it;
			// otherwise only the server that answers speaks.
			if snap.Current || e.Kind == lifecycle.EventMovePublished || e.Kind == lifecycle.EventCurrent {
				b.broadcast(ctx, eventText(e), nil)
			}
		case <-t.C:
			b.announceRequests(ctx)
		}
	}
}

// announceRequests sends every owner each new request, with the buttons to
// decide it, and records that they have been told. While a move is under way
// it waits: the replacement inherits the list and tells them instead.
func (b *Bot) announceRequests(ctx context.Context) {
	snap := b.Manager.Snapshot()
	if !snap.Current || snap.Moving {
		return
	}
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpOverview})
	if err != nil || !resp.OK || resp.Overview == nil {
		if err != nil {
			b.log().Debug("could not read the device list", "error", err)
		}
		return
	}
	var told []string
	for _, w := range resp.Overview.Pending {
		if w.Notified {
			continue
		}
		if b.broadcast(ctx, requestText(w), requestKeyboard(w)) {
			told = append(told, w.ID)
		}
	}
	if len(told) > 0 {
		if r, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpNotified, IDs: told}); err != nil || !r.OK {
			b.log().Warn("could not record that the operator was told", "error", err, "answer", r.Error)
		}
	}
}

// broadcast sends a message to every owner. It reports whether at least one of
// them got it.
func (b *Bot) broadcast(ctx context.Context, text string, kb telegram.Keyboard) bool {
	sent := false
	for _, id := range b.Owners {
		if _, err := b.API.Send(ctx, id, scrubAddresses(text), kb); err != nil {
			b.log().Warn("could not send to an owner", "error", err)
			continue
		}
		sent = true
	}
	return sent
}

func (b *Bot) isOwner(id int64) bool { return slices.Contains(b.Owners, id) }

// handle answers one update, or ignores it if it is not from an owner in their
// own chat with the bot.
func (b *Bot) handle(ctx context.Context, u telegram.Update) {
	switch {
	case u.Message != nil:
		m := u.Message
		if m.From == nil || !b.isOwner(m.From.ID) || m.Chat.Type != "private" {
			return
		}
		b.command(ctx, m.Chat.ID, strings.TrimSpace(m.Text))
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if !b.isOwner(q.From.ID) || q.Message == nil || q.Message.Chat.Type != "private" {
			return
		}
		b.press(ctx, q)
	}
}

var menuCommands = []telegram.Command{
	{Command: "status", Description: "Состояние сервера"},
	{Command: "devices", Description: "Устройства с доступом"},
	{Command: "requests", Description: "Заявки и отказы"},
	{Command: "service", Description: "Обслуживание: переезд, перезапуск, автопереезды"},
}

// command answers a typed message with a new message.
func (b *Bot) command(ctx context.Context, chat int64, text string) {
	cmd, _, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@") // "/status@SomeBot" in a menu tap
	var out view
	switch cmd {
	case "/status":
		out = b.statusView(ctx)
	case "/devices":
		out = b.devicesView(ctx)
	case "/requests":
		out = b.requestsView(ctx)
	case "/service":
		out = b.serviceView()
	default:
		out = menuView()
	}
	b.reply(ctx, chat, out)
}

func (b *Bot) reply(ctx context.Context, chat int64, v view) {
	if _, err := b.API.Send(ctx, chat, scrubAddresses(v.text), v.kb); err != nil {
		b.log().Warn("could not answer the operator", "error", err)
	}
}

// later sends a message from another goroutine, such as the manager's
// callback when an action is done.
func (b *Bot) later(chat int64, text string) {
	b.mu.Lock()
	ctx := b.ctx
	b.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := b.API.Send(sendCtx, chat, scrubAddresses(text), nil); err != nil {
			b.log().Warn("could not tell the operator how an action went", "error", err)
		}
	}()
}

// press answers a button.
func (b *Bot) press(ctx context.Context, q *telegram.CallbackQuery) {
	chat, msg := q.Message.Chat.ID, q.Message.MessageID
	edit := func(v view) {
		if err := b.API.Edit(ctx, chat, msg, scrubAddresses(v.text), v.kb); err != nil {
			b.log().Warn("could not update a message", "error", err)
		}
	}
	notice := ""
	defer func() { _ = b.API.Answer(ctx, q.ID, notice) }()

	op, arg, _ := strings.Cut(q.Data, ":")
	switch op {
	case "m":
		switch arg {
		case "status":
			edit(b.statusView(ctx))
		case "devices":
			edit(b.devicesView(ctx))
		case "requests":
			edit(b.requestsView(ctx))
		case "service":
			edit(b.serviceView())
		default:
			edit(menuView())
		}

	case "ap", "dn", "rv!", "al", "fg":
		if b.Manager.Snapshot().Moving {
			notice = "Идёт переезд. Решения об устройствах — через пару минут, на новом сервере."
			return
		}
		// A button in a list carries ":l" after the id: the list is drawn again.
		// Without it the button came with the request's own message, which now
		// just says what was decided.
		id, from, _ := strings.Cut(arg, ":")
		ops := map[string]string{"ap": node.OpApprove, "dn": node.OpDeny, "rv!": node.OpRevoke, "al": node.OpAllow, "fg": node.OpForget}
		resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: ops[op], ID: id})
		text := decisionText(op, resp, err)
		notice = text
		if (op == "ap" || op == "dn") && from != "l" {
			edit(view{text: text})
		} else {
			edit(b.devicesOrRequests(ctx, op, text))
		}

	case "rv":
		edit(b.confirmRevoke(ctx, arg))

	case "mv":
		edit(confirmView("Переехать на новый сервер сейчас? Приложения переключатся сами, связь пропадёт на несколько секунд.",
			"Да, переехать", "mv!:"+nonce(), "m:service"))
	case "mr":
		edit(b.regionView())
	case "mv!":
		id, region, _ := strings.Cut(arg, ":")
		notice = b.act(chat, lifecycle.Action{Kind: lifecycle.ActMove, ID: id, Region: region},
			"Переезд начнётся в течение минуты. О ходе напишу.")
		edit(b.serviceView())

	case "rs":
		edit(confirmView("Перезапустить туннель? У всех пропадёт связь на несколько секунд.",
			"Да, перезапустить", "rs!:"+nonce(), "m:service"))
	case "rs!":
		notice = b.act(chat, lifecycle.Action{Kind: lifecycle.ActRestartNode, ID: arg}, "")
		edit(b.serviceView())

	case "pa", "pb":
		on := arg == "on"
		a := lifecycle.Action{Kind: lifecycle.ActPolicy}
		if op == "pa" {
			a.AutoAge = &on
		} else {
			a.AutoBlock = &on
		}
		notice = b.act(chat, a, "")
		// The manager applies it a moment later; the menu is drawn from the
		// change itself, so it does not show the state from before.
		edit(b.serviceViewWith(a.AutoAge, a.AutoBlock))

	case "ck":
		notice = b.act(chat, lifecycle.Action{Kind: lifecycle.ActCheck}, "Проверяю, это займёт до минуты…")

	default:
		notice = "Эта кнопка устарела."
	}
}

// act hands an action to the manager and returns the notice for the button.
// The outcome arrives later as a message.
func (b *Bot) act(chat int64, a lifecycle.Action, accepted string) string {
	kind := a.Kind
	a.Done = func(r lifecycle.Result) {
		if text := resultText(kind, r); text != "" {
			b.later(chat, text)
		}
	}
	if err := b.Manager.Do(a); err != nil {
		if errors.Is(err, lifecycle.ErrBusy) {
			return "Сервер занят, повторите через минуту."
		}
		return "Не получилось: " + err.Error()
	}
	if accepted == "" {
		return "Принято."
	}
	return accepted
}

func (b *Bot) devicesOrRequests(ctx context.Context, op, said string) view {
	var v view
	if op == "rv!" {
		v = b.devicesView(ctx)
	} else {
		v = b.requestsView(ctx)
	}
	v.text = said + "\n\n" + v.text
	return v
}

func (b *Bot) overview(ctx context.Context) (*node.ControlOverview, error) {
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpOverview})
	if err != nil {
		return nil, err
	}
	if !resp.OK || resp.Overview == nil {
		return nil, errors.New(resp.Error)
	}
	return resp.Overview, nil
}

// nonce is a one-time id for a confirmation button: pressing it twice, or on
// two servers, does the thing once.
func nonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var addressPattern = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`)

// scrubAddresses keeps IPv4 addresses out of anything sent: an error from deep
// down may quote one, and Telegram is not the place for it.
func scrubAddresses(s string) string {
	return addressPattern.ReplaceAllString(s, "‹адрес›")
}
