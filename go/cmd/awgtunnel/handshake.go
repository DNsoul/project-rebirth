package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// newestHandshake reads the answer to a UAPI "get=1" request and returns the time
// of the newest handshake over all peers, in unix seconds, or 0 when no peer has
// shaken hands yet. The answer is lines of key=value up to an empty line, and it
// ends with errno=0 when the request worked.
func newestHandshake(answer io.Reader) (int64, error) {
	s, err := readStats(answer)
	return s.handshake, err
}

// stats is what a tunnel says about itself: its newest handshake (unix
// seconds, 0 for none) and the bytes received and sent, over all peers.
type stats struct {
	handshake int64
	rx, tx    uint64
}

// String is what "awgtunnel stats" prints: a line for each, name and number.
func (s stats) String() string {
	return fmt.Sprintf("handshake %d\nrx %d\ntx %d", s.handshake, s.rx, s.tx)
}

// readStats reads the answer to a UAPI "get=1" request.
func readStats(answer io.Reader) (stats, error) {
	var s stats
	scanner := bufio.NewScanner(answer)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "errno":
			if v != "0" {
				return stats{}, fmt.Errorf("the tunnel answered errno=%s", v)
			}
		case "last_handshake_time_sec":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > s.handshake {
				s.handshake = n
			}
		case "rx_bytes":
			n, _ := strconv.ParseUint(v, 10, 64)
			s.rx += n
		case "tx_bytes":
			n, _ := strconv.ParseUint(v, 10, 64)
			s.tx += n
		}
	}
	return s, scanner.Err()
}

// command is what the program was asked to do.
type command struct {
	name string
	arg  string
}

// usage lists what the program does. It is run by the service manager as
// "awgtunnel /tunnelservice <conf>", and by Rebirth with the others.
const usage = `usage:
  awgtunnel install <conf>        install the tunnel service for a configuration and start it
  awgtunnel uninstall <name>      stop and remove the tunnel service called <name>
  awgtunnel handshake <name>      print the time of the newest handshake (unix seconds, 0 if none)
  awgtunnel stats <name>          print the newest handshake and the bytes received and sent
  awgtunnel log <folder>          print the tunnel's log kept in <folder>
  awgtunnel version
  awgtunnel /tunnelservice <conf> (used by the service manager)`

// parseArgs checks the command line: one command, with the one argument it takes.
func parseArgs(args []string) (command, error) {
	if len(args) == 0 {
		return command{}, fmt.Errorf("no command given\n%s", usage)
	}
	name, rest := args[0], args[1:]
	switch name {
	case "install", "uninstall", "handshake", "stats", "log", "/tunnelservice":
		if len(rest) != 1 || rest[0] == "" {
			return command{}, fmt.Errorf("%s takes one argument\n%s", name, usage)
		}
		return command{name, rest[0]}, nil
	case "version":
		if len(rest) != 0 {
			return command{}, fmt.Errorf("version takes no argument\n%s", usage)
		}
		return command{name: name}, nil
	}
	return command{}, fmt.Errorf("unknown command %q\n%s", name, usage)
}
