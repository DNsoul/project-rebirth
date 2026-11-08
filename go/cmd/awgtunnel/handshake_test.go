package main

import (
	"strings"
	"testing"
)

func TestNewestHandshake(t *testing.T) {
	cases := []struct {
		name    string
		answer  string
		want    int64
		wantErr bool
	}{
		{
			name:   "one peer",
			answer: "private_key=aa\nlisten_port=0\npublic_key=bb\nlast_handshake_time_sec=1762600000\nlast_handshake_time_nsec=5\nerrno=0\n\n",
			want:   1762600000,
		},
		{
			name:   "the newest of several peers",
			answer: "public_key=a\nlast_handshake_time_sec=100\npublic_key=b\nlast_handshake_time_sec=300\npublic_key=c\nlast_handshake_time_sec=200\nerrno=0\n\n",
			want:   300,
		},
		{
			name:   "no handshake yet is zero",
			answer: "public_key=a\nlast_handshake_time_sec=0\nlast_handshake_time_nsec=0\nerrno=0\n\n",
			want:   0,
		},
		{
			name:   "no peers is zero",
			answer: "private_key=aa\nerrno=0\n\n",
			want:   0,
		},
		{
			name:   "nothing after the empty line is read",
			answer: "last_handshake_time_sec=7\nerrno=0\n\nlast_handshake_time_sec=999\n",
			want:   7,
		},
		{
			name:   "lines that are not numbers are skipped",
			answer: "last_handshake_time_sec=abc\nlast_handshake_time_sec=42\nerrno=0\n\n",
			want:   42,
		},
		{
			name:    "an error from the tunnel is an error",
			answer:  "errno=-22\n\n",
			wantErr: true,
		},
		{
			name:   "an answer cut short still gives what was read",
			answer: "last_handshake_time_sec=11\n",
			want:   11,
		},
		{name: "an empty answer is zero", answer: "", want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := newestHandshake(strings.NewReader(c.answer))
			if (err != nil) != c.wantErr {
				t.Fatalf("error = %v, want error: %v", err, c.wantErr)
			}
			if got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	good := map[string]command{
		"install C:/x/awgr-a.conf":        {"install", "C:/x/awgr-a.conf"},
		"uninstall awgr-a":                {"uninstall", "awgr-a"},
		"handshake awgr-b":                {"handshake", "awgr-b"},
		"log C:/x":                        {"log", "C:/x"},
		"/tunnelservice C:/x/awgr-a.conf": {"/tunnelservice", "C:/x/awgr-a.conf"},
		"version":                         {name: "version"},
	}
	for line, want := range good {
		got, err := parseArgs(strings.Fields(line))
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v; want %+v", line, got, err, want)
		}
	}
	for _, line := range []string{
		"",
		"install",
		"install a b",
		"uninstall",
		"handshake x y",
		"version now",
		"/tunnelservice",
		"explode awgr-a",
	} {
		if got, err := parseArgs(strings.Fields(line)); err == nil {
			t.Errorf("%q should be refused, got %+v", line, got)
		}
	}
	if _, err := parseArgs([]string{"install", ""}); err == nil {
		t.Error("an empty argument should be refused")
	}
}
