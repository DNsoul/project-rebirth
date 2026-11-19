// Package usage counts where the devices' traffic goes: how much each device
// sent and received, and to which sites, kept as sums by the hour for two days
// and by the day for five weeks.
//
// It is the one place in the project that writes down sites, and it does so
// only as far as the operator needs to see what eats the traffic allowance: a
// site is the registrable domain or a well-known service ("YouTube"), never the
// full name, and only sums are kept, never when or how often a site was asked
// for. Nothing here is logged.
package usage

import (
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Labels for traffic that has no site.
const (
	// SiteServer is traffic with the server itself: the resolver and the
	// registration service.
	SiteServer = "сервер"

	// SiteTelegram is traffic to Telegram's own networks, which the clients
	// route through the tunnel by address rather than by name.
	SiteTelegram = "Telegram"

	// SiteUnknown is traffic to a stand-in whose name is not known: handed out
	// before names were kept, and still used by a connection opened then.
	SiteUnknown = "неизвестно"

	// SiteElsewhere is traffic to a real address that is neither the
	// server's nor Telegram's: a client that sends more than the stand-ins
	// through the tunnel.
	SiteElsewhere = "другие адреса"

	// SiteOther is what is left once a list has been cut to its largest
	// entries.
	SiteOther = "остальное"
)

// services groups the domains that make up one service the operator knows
// by name: a video is served from googlevideo.com, not from youtube.com.
var services = map[string]string{
	"youtube.com": "YouTube", "googlevideo.com": "YouTube", "ytimg.com": "YouTube",
	"ggpht.com": "YouTube", "youtube-nocookie.com": "YouTube", "youtu.be": "YouTube",

	"instagram.com": "Instagram", "cdninstagram.com": "Instagram",
	"facebook.com": "Facebook", "fbcdn.net": "Facebook", "fbsbx.com": "Facebook", "facebook.net": "Facebook",
	"whatsapp.com": "WhatsApp", "whatsapp.net": "WhatsApp",

	"tiktok.com": "TikTok", "tiktokv.com": "TikTok", "tiktokcdn.com": "TikTok",
	"tiktokcdn-eu.com": "TikTok", "ttwstatic.com": "TikTok", "byteoversea.com": "TikTok", "ibyteimg.com": "TikTok",

	"telegram.org": "Telegram", "t.me": "Telegram", "telegram.me": "Telegram", "cdn-telegram.org": "Telegram",

	"twitch.tv": "Twitch", "ttvnw.net": "Twitch", "jtvnw.net": "Twitch",

	"discord.com": "Discord", "discord.gg": "Discord", "discordapp.com": "Discord",
	"discordapp.net": "Discord", "discord.media": "Discord",

	"steampowered.com": "Steam", "steamcontent.com": "Steam", "steamserver.net": "Steam",
	"steamstatic.com": "Steam", "steamcommunity.com": "Steam",

	"netflix.com": "Netflix", "nflxvideo.net": "Netflix", "nflximg.net": "Netflix", "nflxso.net": "Netflix",

	"spotify.com": "Spotify", "scdn.co": "Spotify", "spotifycdn.com": "Spotify",

	"twitter.com": "X (Twitter)", "x.com": "X (Twitter)", "twimg.com": "X (Twitter)",

	"windowsupdate.com": "Windows Update",

	"chatgpt.com": "ChatGPT", "openai.com": "ChatGPT", "oaiusercontent.com": "ChatGPT",
}

// SiteOf is the site a name belongs to: the service it is part of, if it is
// a known one, or else its registrable domain ("rr3---sn-x.googlevideo.com"
// is YouTube, "cdn.example.co.uk" is example.co.uk).
func SiteOf(name string) string {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "" {
		return SiteUnknown
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		// A bare suffix or something that is not a name: keep it as it is,
		// it is short anyway.
		site = name
	}
	if s, ok := services[site]; ok {
		return s
	}
	return site
}
