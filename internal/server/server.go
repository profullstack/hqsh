// Package server is the remote side: the daemon that owns a session's PTY and
// the attach bridge that ssh runs. See docs/protocol.md.
package server

import (
	"errors"
	"os"
	"path/filepath"
)

// DefaultBuffer is how much output a session keeps for resuming (4 MiB).
const DefaultBuffer = 4 << 20

// ErrNotImplemented marks the parts of the stub still to be built.
var ErrNotImplemented = errors.New("hqsh: not implemented yet")

// SocketPath is where session's daemon listens:
// $XDG_RUNTIME_DIR/hqsh/<session>.sock, else ~/.local/state/hqsh/.
func SocketPath(session string) (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "hqsh", session+".sock"), nil
}

// Attach bridges stdin/stdout (the ssh pipe) to the session's daemon,
// starting the daemon first when it is not running.
//
// TODO:
//   - dial SocketPath(session); on ENOENT/ECONNREFUSED fork `hqsh server daemon <session>`
//     detached (setsid), wait for the socket, dial again
//   - copy frames both ways until either side closes
func Attach(session string) error {
	_ = session
	return ErrNotImplemented
}

// Daemon owns the PTY for one session and serves attaches on its socket.
//
// TODO:
//   - start $SHELL -l in a PTY (github.com/creack/pty) with TERM from the first HELLO
//   - read PTY output into a ring.Buffer, sending OUTPUT frames to the attached client
//   - one client at a time: a new attach replaces the old (send it nothing more)
//   - HELLO: reply WELCOME, replay ring.Since(lastSeq); on a gap set the flag and
//     nudge a redraw by resizing rows-1 then rows
//   - INPUT -> PTY, RESIZE -> pty.Setsize, ACK -> ring.Trim, PING -> PONG
//   - shell exits: send EXIT with its status, remove the socket, quit
func Daemon(session string) error {
	_ = session
	return ErrNotImplemented
}
