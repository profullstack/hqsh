// Package client is the local side: puts your terminal in raw mode, runs
// `ssh host hqsh server attach <session>`, and keeps the session going across
// disconnects. See docs/protocol.md.
package client

import (
	"errors"
	"time"
)

// ErrNotImplemented marks the parts of the stub still to be built.
var ErrNotImplemented = errors.New("hqsh: not implemented yet")

// Options for a connection.
type Options struct {
	Host    string   // [user@]host, as ssh takes it
	Session string   // default "main"
	SSHArgs []string // extra ssh flags (-p, -i, ...)
	Remote  string   // the hqsh binary on the host, default "hqsh"
}

// Backoff is the reconnect delay for attempt n (0-based): 0.5s doubling to 10s.
func Backoff(n int) time.Duration {
	d := 500 * time.Millisecond
	for i := 0; i < n && d < 10*time.Second; i++ {
		d *= 2
	}
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// SSHCommand is the argv that reaches the remote attach.
func SSHCommand(o Options) []string {
	remote := o.Remote
	if remote == "" {
		remote = "hqsh"
	}
	session := o.Session
	if session == "" {
		session = "main"
	}
	argv := append([]string{"ssh", "-T", "-o", "ServerAliveInterval=10"}, o.SSHArgs...)
	return append(argv, o.Host, remote, "server", "attach", session)
}

// Run connects and stays connected until the session exits or the user quits
// with Ctrl-^ then '.'.
//
// TODO:
//   - golang.org/x/term: raw mode, restore on exit; size from term.GetSize
//   - loop: start SSHCommand, send HELLO{lastSeq, cols, rows}
//   - OUTPUT: write to stdout, remember seq, ACK every ~64 KiB
//   - stdin -> INPUT frames (watch for Ctrl-^ .), SIGWINCH -> RESIZE
//   - PING every 5s; no PONG in 15s, EOF or a write error -> reconnect after Backoff(n),
//     showing "hqsh: reconnecting…" in the window title (OSC 2), never on screen
//   - WELCOME with the gap flag: clear the screen before the replay
//   - EXIT: restore the terminal and return the shell's status
func Run(o Options) error {
	_ = o
	return ErrNotImplemented
}
