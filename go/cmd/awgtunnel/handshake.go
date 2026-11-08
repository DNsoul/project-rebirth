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
	var newest int64
	scanner := bufio.NewScanner(answer)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "errno="); ok {
			if v != "0" {
				return 0, fmt.Errorf("the tunnel answered errno=%s", v)
			}
			continue
		}
		v, ok := strings.CutPrefix(line, "last_handshake_time_sec=")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > newest {
			newest = n
		}
	}
	return newest, scanner.Err()
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
	case "install", "uninstall", "handshake", "log", "/tunnelservice":
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
