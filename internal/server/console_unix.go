//go:build !windows

package server

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/creack/pty"
)

type unixConsole struct{ *os.File }

func (c unixConsole) Resize(cols, rows uint16) error {
	return pty.Setsize(c.File, &pty.Winsize{Cols: cols, Rows: rows})
}

// startShell runs the user's login shell ($HQSH_SHELL, else $SHELL, else
// /bin/sh) in their home directory on a new PTY.
func startShell(session, term string, cols, rows uint16) (console, func() int, error) {
	shell := os.Getenv("HQSH_SHELL")
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	cmd.Env = shellEnv(os.Environ(), usableTerm(term), session)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, nil, err
	}
	return unixConsole{ptmx}, func() int {
		_ = cmd.Wait()
		return exitCode(cmd.ProcessState)
	}, nil
}

func lockSession(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func chmodSocket(path string) error { return os.Chmod(path, 0o600) }

func isDead(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func shellEnv(env []string, term, session string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") || strings.HasPrefix(kv, "HQSH_SESSION=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERM="+term, "HQSH_SESSION="+session)
}

// usableTerm keeps the client's TERM when this host has a terminfo entry for
// it (xterm-kitty often is missing), else falls back to xterm-256color.
func usableTerm(term string) string {
	const fallback = "xterm-256color"
	if term == "" || strings.ContainsAny(term, "/\\\x00") || strings.HasPrefix(term, ".") {
		return fallback
	}
	var dirs []string
	if t := os.Getenv("TERMINFO"); t != "" {
		dirs = append(dirs, t)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".terminfo"))
	}
	if td := os.Getenv("TERMINFO_DIRS"); td != "" {
		dirs = append(dirs, filepath.SplitList(td)...)
	}
	dirs = append(dirs, "/etc/terminfo", "/lib/terminfo", "/usr/share/terminfo", "/usr/lib/terminfo", "/usr/share/lib/terminfo", "/opt/homebrew/share/terminfo", "/usr/local/share/terminfo")
	anyDir := false
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		anyDir = true
		for _, sub := range []string{term[:1], fmt.Sprintf("%x", term[0])} {
			if _, err := os.Stat(filepath.Join(dir, sub, term)); err == nil {
				return term
			}
		}
	}
	if !anyDir {
		return term // no terminfo database to check against; trust the client
	}
	return fallback
}
