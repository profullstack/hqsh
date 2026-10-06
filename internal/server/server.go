// Package server is the remote side: the daemon that owns a session's PTY and
// the attach bridge that ssh runs. See docs/protocol.md.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBuffer is how much output a session keeps for resuming (4 MiB).
const DefaultBuffer = 4 << 20

// SocketDir is where daemons listen: $XDG_RUNTIME_DIR/hqsh, else
// ~/.local/state/hqsh.
func SocketDir() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "hqsh"), nil
}

// SocketPath is where session's daemon listens: <SocketDir>/<session>.sock.
func SocketPath(session string) (string, error) {
	dir, err := SocketDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, session+".sock"), nil
}

// ValidSession rejects names that would escape the socket directory or not
// fit in a socket path.
func ValidSession(session string) error {
	if session == "" || len(session) > 64 || session == "." || session == ".." ||
		strings.ContainsAny(session, "/\\\x00") || strings.HasPrefix(session, ".") {
		return fmt.Errorf("hqsh: bad session name %q (use letters, digits, - and _)", session)
	}
	return nil
}

// SessionInfo is one live session, as `hqsh server list --json` prints it.
type SessionInfo struct {
	Name     string `json:"name"`
	Attached bool   `json:"attached"` // at least one client
	Clients  int    `json:"clients"`  // how many clients are attached
}

// WriteList prints sessions: a JSON array (always an array, [] when empty)
// or one "name<TAB>attached|detached" line each ("attached (N clients)" when
// more than one is).
func WriteList(w io.Writer, sessions []SessionInfo, asJSON bool) error {
	if asJSON {
		if sessions == nil {
			sessions = []SessionInfo{}
		}
		b, err := json.Marshal(sessions)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	}
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(w, "no sessions")
		return err
	}
	for _, s := range sessions {
		state := "detached"
		if s.Attached {
			state = "attached"
		}
		if s.Clients > 1 {
			state = fmt.Sprintf("attached (%d clients)", s.Clients)
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\n", s.Name, state); err != nil {
			return err
		}
	}
	return nil
}

// winsize is one attached client's terminal size.
type winsize struct{ cols, rows uint16 }

// minSize is the PTY size for several clients, as tmux does it by default:
// the smallest cols and the smallest rows among them (each taken on its
// own). Sizes with a zero side are ignored; ok is false when none is left.
func minSize(sizes []winsize) (cols, rows uint16, ok bool) {
	for _, s := range sizes {
		if s.cols == 0 || s.rows == 0 {
			continue
		}
		if !ok || s.cols < cols {
			cols = s.cols
		}
		if !ok || s.rows < rows {
			rows = s.rows
		}
		ok = true
	}
	return cols, rows, ok
}

// paceWindow is how far (in bytes) the shell may run ahead of the most
// caught-up client before the daemon stops reading the PTY. Half the ring,
// so that client never falls off it. With one client this is plain
// backpressure, as in 0.1; with several, the slower ones are left behind
// (and dropped when they fall off the ring) instead of slowing everyone.
const paceWindow = DefaultBuffer / 2

// mustWait reports whether the PTY reader should wait: some client is
// attached and even the most caught-up one is more than window bytes
// behind. behind holds each client's backlog in bytes.
func mustWait(behind []int, window int) bool {
	if len(behind) == 0 {
		return false
	}
	for _, b := range behind {
		if b <= window {
			return false
		}
	}
	return true
}

// minAck is how far the output buffer may be trimmed: the lowest seq every
// attached client has acknowledged, so nothing a lagging client still needs
// is dropped early (the buffer's byte budget still bounds it). ok is false
// when no client is attached: then nothing is trimmed, and the budget alone
// decides what a returning client can resume.
func minAck(acks []uint64) (seq uint64, ok bool) {
	for i, a := range acks {
		if i == 0 || a < seq {
			seq = a
		}
	}
	return seq, len(acks) > 0
}
