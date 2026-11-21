package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"awgrotator/internal/node"
	"awgrotator/internal/telegram"
	"awgrotator/internal/usage"
)

// Sites sent direct: the operator's list of sites whose traffic goes over the
// devices' own connections instead of the server's, so it does not count
// against the server's allowance. Sites are put on it from the traffic list
// or by name, and taken off from the list or from the site.

// directPerPage is how many sites one page of the list shows.
const directPerPage = 10

// maxData is the most button data Telegram carries.
const maxData = 64

// directMark marks a site sent direct in a list of sites. It is one
// character, so the columns of the monospace block stay in line.
var directMark = "→"

// directLegend explains the mark, under a list that has one.
func directLegend() string { return directMark + " напрямую, мимо сервера" }

// directList reads the operator's list from the node.
func (b *Bot) directList(ctx context.Context) ([]string, error) {
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpDirect})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return resp.Direct, nil
}

// siteRef is a site as button data after prefix: the name itself when it
// fits, or else a short key the bot remembers until it restarts. Sites are
// registrable domains, so almost all of them fit.
func (b *Bot) siteRef(prefix, site string) string {
	if len(prefix)+len(site) <= maxData {
		return prefix + site
	}
	sum := sha256.Sum256([]byte(site))
	key := "~" + hex.EncodeToString(sum[:8])
	b.mu.Lock()
	if b.refs == nil {
		b.refs = map[string]string{}
	}
	b.refs[key] = site
	b.mu.Unlock()
	return prefix + key
}

// siteOf reads a site back from button data, reporting false for a key the
// bot no longer remembers.
func (b *Bot) siteOf(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "~") {
		return arg, arg != ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	site, ok := b.refs[arg]
	return site, ok
}

// directView is one page of the list.
func (b *Bot) directView(ctx context.Context, page int) view {
	back := []telegram.Button{btn("‹ Трафик", "tr:d")}
	add := []telegram.Button{btn("Добавить", "dq")}
	names, err := b.directList(ctx)
	if err != nil {
		return view{text: "Список «напрямую» сейчас не читается: " + firstLine(err.Error()), kb: telegram.Keyboard{back}}
	}
	if len(names) == 0 {
		return view{text: "Напрямую пока ничего не идёт.", kb: telegram.Keyboard{add, back}}
	}

	pages := (len(names) + directPerPage - 1) / directPerPage
	page = min(max(page, 0), pages-1)
	from := page * directPerPage
	shown := names[from:min(from+directPerPage, len(names))]

	rows := make([][]string, 0, len(shown))
	buttons := make([]telegram.Button, 0, len(shown))
	for i, name := range shown {
		n := strconv.Itoa(from + i + 1)
		rows = append(rows, []string{n, name})
		buttons = append(buttons, btn(n, b.siteRef("dd:", name)))
	}
	kb := rowsOf(buttons, 5)
	var nav []telegram.Button
	if page > 0 {
		nav = append(nav, btn("‹", "dp:"+strconv.Itoa(page-1)))
	}
	if page < pages-1 {
		nav = append(nav, btn("›", "dp:"+strconv.Itoa(page+1)))
	}
	if len(nav) > 0 {
		kb = append(kb, nav)
	}
	kb = append(kb, add, back)
	// Numbers as wide as the traffic list's, until a third digit is needed.
	width := max(2, len(strconv.Itoa(from+len(shown))))
	return view{text: fmt.Sprintf("Напрямую, мимо сервера: %d.\n", len(names)) + telegram.Mono(columns(rows, -width, 30)), kb: kb}
}

// directItemView is one site of the list.
func (b *Bot) directItemView(ctx context.Context, site string) view {
	back := []telegram.Button{btn("‹ Напрямую", "dp:0")}
	names, err := b.directList(ctx)
	if err != nil {
		return view{text: "Список «напрямую» сейчас не читается: " + firstLine(err.Error()), kb: telegram.Keyboard{back}}
	}
	head := site
	if s := usage.ServiceOf(usage.SiteOf(site)); s != "" {
		head += " · " + s
	}
	if !slices.Contains(names, site) {
		return view{text: head + "\nЭтого сайта уже нет в списке.", kb: telegram.Keyboard{back}}
	}
	return view{text: head + "\nИдёт напрямую, мимо сервера.", kb: telegram.Keyboard{
		{btn("Вернуть через сервер", b.siteRef("dx:", site))}, back,
	}}
}

// sendDirect puts a site on the list, and says how that went in a line for
// a notice: empty when it went.
func (b *Bot) sendDirect(ctx context.Context, site string) string {
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpDirectAdd, Name: site})
	if err == nil && resp.OK {
		return ""
	}
	return directRefusal(resp, err)
}

// takeBack takes a site off the list.
func (b *Bot) takeBack(ctx context.Context, site string) string {
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpDirectRemove, Name: site})
	if err == nil && resp.OK {
		return ""
	}
	return directRefusal(resp, err)
}

// directRefusal is why the list did not change.
func directRefusal(resp node.ControlResponse, err error) string {
	switch {
	case err != nil:
		return "Не получилось: узел не отвечает."
	case resp.Reason == node.DirectTooBroad:
		return "Слишком широкое имя: под ним тысячи разных сайтов."
	case resp.Reason == node.DirectNotAName:
		return "Это не похоже на имя сайта."
	case resp.Reason == node.DirectAlwaysTunnel:
		return "Это имя всегда идёт через сервер: через него следят, какие страницы открывают."
	case resp.Reason == node.DirectFull:
		return "Список полон."
	case resp.Missing:
		return "Этого сайта уже нет в списке."
	}
	return "Не получилось: " + firstLine(resp.Error)
}

// switchedNotice is the notice after a site was sent direct.
const switchedNotice = "Устройства перейдут на прямой путь за минуту-две."

// directPrompt asks for a site to send direct, answered by a reply.
func (b *Bot) directPrompt(ctx context.Context, chat int64) string {
	m, err := b.API.Ask(ctx, chat,
		"Какой сайт пустить напрямую? Ответьте на это сообщение его именем, например steamcontent.com. Поддомены пойдут вместе с ним.",
		"example.com")
	if err != nil {
		b.log().Warn("could not ask for a site", "error", err)
		return "Не получилось спросить."
	}
	b.mu.Lock()
	if b.directAsks == nil {
		b.directAsks = map[int64]bool{}
	}
	b.directAsks[m.MessageID] = true
	b.mu.Unlock()
	return ""
}

// directAnswered carries out an answer to directPrompt, if m is one, and
// reports whether it was.
func (b *Bot) directAnswered(ctx context.Context, m *telegram.Message) bool {
	if m.ReplyTo == nil {
		return false
	}
	b.mu.Lock()
	ok := b.directAsks[m.ReplyTo.MessageID]
	delete(b.directAsks, m.ReplyTo.MessageID)
	b.mu.Unlock()
	if !ok {
		return false
	}
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpDirectAdd, Name: hostOf(m.Text)})
	if err != nil || !resp.OK {
		b.reply(ctx, m.Chat.ID, view{text: directRefusal(resp, err)})
		return true
	}
	v := b.directView(ctx, 0)
	v.text = fmt.Sprintf("%s идёт напрямую. %s\n\n%s", resp.Name, switchedNotice, v.text)
	b.reply(ctx, m.Chat.ID, v)
	return true
}

// hostOf takes the name out of what the operator pasted, which is often an
// address from the browser: "https://store.example.com/app/1" is
// store.example.com.
func hostOf(s string) string {
	s = strings.TrimSpace(s)
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	s, _, _ = strings.Cut(s, "/")
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if host, port, ok := strings.Cut(s, ":"); ok && port != "" && strings.Trim(port, "0123456789") == "" {
		s = host
	}
	return s
}

// pressDirect answers the buttons of the traffic sites and the list.
func (b *Bot) pressDirect(ctx context.Context, chat int64, op, arg string, edit func(view)) (notice string) {
	stale := "Список устарел, откройте его заново."
	switch op {
	case "tv":
		edit(b.trafficDevicesView(ctx, periodOf(arg)))
	case "ts", "da", "dr":
		pk, ref, _ := strings.Cut(arg, ":")
		site, ok := b.siteOf(ref)
		if !ok {
			return stale
		}
		period := periodOf(pk)
		switch op {
		case "da":
			if notice = b.sendDirect(ctx, site); notice == "" {
				notice = switchedNotice
			}
		case "dr":
			if notice = b.takeBack(ctx, site); notice == "" {
				notice = site + " снова идёт через сервер."
			}
		}
		edit(b.siteView(ctx, period, site))
	case "dp":
		n, _ := strconv.Atoi(arg)
		edit(b.directView(ctx, n))
	case "dd":
		site, ok := b.siteOf(arg)
		if !ok {
			return stale
		}
		edit(b.directItemView(ctx, site))
	case "dx":
		site, ok := b.siteOf(arg)
		if !ok {
			return stale
		}
		if notice = b.takeBack(ctx, site); notice == "" {
			notice = site + " снова идёт через сервер."
		}
		edit(b.directView(ctx, 0))
	case "dq":
		return b.directPrompt(ctx, chat)
	}
	return notice
}
