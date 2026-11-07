package remote

import "testing"

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/etc/awgnode/config.json": `'/etc/awgnode/config.json'`,
		"plain":                    `'plain'`,
		"with space":               `'with space'`,
		`it's`:                     `'it'\''s'`,
		`; rm -rf /`:               `'; rm -rf /'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParentDir(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/awgnode":   "/usr/local/bin",
		"/etc/awgnode/config.json": "/etc/awgnode",
		"/top":                     "/",
		"bare":                     "/",
	}
	for in, want := range cases {
		if got := parentDir(in); got != want {
			t.Errorf("parentDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstWord(t *testing.T) {
	if got := firstWord("sh /root/install.sh"); got != "sh" {
		t.Errorf("firstWord = %q", got)
	}
	if got := firstWord("uptime"); got != "uptime" {
		t.Errorf("firstWord = %q", got)
	}
}

func TestJoinOutput(t *testing.T) {
	cases := []struct{ stdout, stderr, want string }{
		{"out", "", "out"},
		{"", "err", "err"},
		{"out", "err", "out\nerr"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := joinOutput(tc.stdout, tc.stderr); got != tc.want {
			t.Errorf("joinOutput(%q, %q) = %q, want %q", tc.stdout, tc.stderr, got, tc.want)
		}
	}
}

func TestFingerprintOfGarbage(t *testing.T) {
	if got := fingerprint("not a key"); got != "(unreadable)" {
		t.Errorf("fingerprint = %q", got)
	}
}
