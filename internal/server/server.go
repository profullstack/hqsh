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
	Attached bool   `json:"attached"`
}

// WriteList prints sessions: a JSON array (always an array, [] when empty)
// or one "name<TAB>attached|detached" line each.
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
		if _, err := fmt.Fprintf(w, "%s\t%s\n", s.Name, state); err != nil {
			return err
		}
	}
	return nil
}
