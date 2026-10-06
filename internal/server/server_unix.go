//go:build !windows

package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/profullstack/hqsh/internal/proto"
	"github.com/profullstack/hqsh/internal/ring"
)

const (
	startWait    = 3 * time.Second  // how long attach waits for a new daemon's socket
	writeTimeout = 5 * time.Second  // a client that cannot take output this long is dropped
	helloTimeout = 30 * time.Second // a daemon nobody says HELLO to gives up
	socketCheck  = 5 * time.Second  // how often the daemon checks its socket file still exists
)

// daemonCommand is the process attach starts when no daemon is listening.
// Tests swap it to re-exec the test binary.
var daemonCommand = func(session string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, "server", "daemon", session), nil
}

// Attach bridges stdin/stdout (the ssh pipe) to the session's daemon,
// starting the daemon first when it is not running.
func Attach(session string) error {
	return AttachIO(session, os.Stdin, os.Stdout)
}

// AttachIO is Attach over any pair of streams. It returns when either side
// closes: the client went away, or the daemon dropped this connection (a
// newer attach took over, or the shell exited).
func AttachIO(session string, in io.Reader, out io.Writer) error {
	if err := ValidSession(session); err != nil {
		return err
	}
	path, err := SocketPath(session)
	if err != nil {
		return err
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		if c, err = startAndDial(session, path); err != nil {
			return err
		}
	}
	defer c.Close()
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(out, c)
		done <- err
	}()
	go func() {
		_, err := io.Copy(c, in)
		done <- err
	}()
	<-done
	return nil
}

func startAndDial(session, path string) (net.Conn, error) {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	cmd, err := daemonCommand(session)
	if err != nil {
		return nil, err
	}
	// Its own session: no controlling terminal, and the ssh hangup that
	// ends this attach never reaches it. stdio stays nil, i.e. /dev/null.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("hqsh: starting the daemon: %w", err)
	}
	// Reap it if it exits while we live (a daemon that loses a start race
	// exits at once; the winner's socket may still be coming up, so keep
	// dialing until the deadline either way).
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(startWait)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("hqsh: the daemon for %q did not start: %w", session, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// List returns the live sessions, oldest name first, and removes sockets no
// daemon answers on.
func List() ([]SessionInfo, error) {
	dir, err := SocketDir()
	if err != nil {
		return nil, err
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sock"))
	sort.Strings(paths)
	var out []SessionInfo
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".sock")
		attached, err := status(p)
		if err != nil {
			if isDead(err) {
				os.Remove(p)
			}
			continue
		}
		out = append(out, SessionInfo{Name: name, Attached: attached})
	}
	return out, nil
}

func isDead(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// status asks a daemon whether a client is attached.
func status(path string) (bool, error) {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if err := proto.Write(c, proto.Frame{Type: proto.Status}); err != nil {
		return false, err
	}
	f, err := proto.Read(c)
	if err != nil {
		return false, err
	}
	if f.Type != proto.Status || len(f.Payload) < 1 {
		return false, fmt.Errorf("hqsh: unexpected status reply")
	}
	return f.Payload[0]&1 != 0, nil
}

// daemon is one session: a PTY, its output ring, and at most one client.
type daemon struct {
	session string
	path    string
	ring    *ring.Buffer

	mu      sync.Mutex // guards everything below, and all writes to cur
	cur     net.Conn
	ptmx    *os.File
	started bool
	ln      net.Listener

	exited chan struct{} // closed once the shell is gone and EXIT went out
}

// Daemon owns the PTY for one session and serves attaches on its socket. It
// returns when the shell exits (or when another daemon already owns the
// session, or nobody ever attaches).
func Daemon(session string) error {
	if err := ValidSession(session); err != nil {
		return err
	}
	path, err := SocketPath(session)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	// One daemon per session: whoever holds the lock owns the socket.
	lock, err := os.OpenFile(strings.TrimSuffix(path, ".sock")+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil // another daemon has it
	}
	d := &daemon{session: session, path: path, ring: ring.New(DefaultBuffer), exited: make(chan struct{})}
	if err := d.listen(); err != nil {
		return err
	}
	go d.watchSocket()
	select {
	case <-d.exited:
	case <-time.After(helloTimeout):
		d.mu.Lock()
		started := d.started
		d.mu.Unlock()
		if !started {
			d.shutdown()
			return nil
		}
		<-d.exited
	}
	return nil
}

func (d *daemon) listen() error {
	os.Remove(d.path) // stale: we hold the lock, so no daemon is behind it
	ln, err := net.Listen("unix", d.path)
	if err != nil {
		return err
	}
	if err := os.Chmod(d.path, 0o600); err != nil {
		ln.Close()
		return err
	}
	d.mu.Lock()
	old := d.ln
	d.ln = ln
	d.mu.Unlock()
	if old != nil {
		old.(*net.UnixListener).SetUnlinkOnClose(false)
		old.Close()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(c)
		}
	}()
	return nil
}

// watchSocket brings the socket back when its file disappears, e.g. systemd
// removing /run/user/UID after the user's last login session ends.
func (d *daemon) watchSocket() {
	t := time.NewTicker(socketCheck)
	defer t.Stop()
	for {
		select {
		case <-d.exited:
			return
		case <-t.C:
			if _, err := os.Stat(d.path); errors.Is(err, os.ErrNotExist) {
				if ensureDir(filepath.Dir(d.path)) == nil {
					_ = d.listen()
				}
			}
		}
	}
}

func (d *daemon) serve(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := proto.Read(c)
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	switch f.Type {
	case proto.Status:
		d.mu.Lock()
		var flags byte
		if d.cur != nil {
			flags = 1
		}
		d.mu.Unlock()
		_ = proto.Write(c, proto.Frame{Type: proto.Status, Payload: []byte{flags}})
		return
	case proto.Hello:
	default:
		return
	}
	h, err := proto.DecodeHello(f.Payload)
	if err != nil {
		return
	}
	if err := d.ensureShell(h); err != nil {
		_ = proto.Write(c, proto.Frame{Type: proto.Exit, Payload: proto.EncodeExit(127)})
		return
	}

	d.mu.Lock()
	if d.cur != nil {
		d.cur.Close() // a newer attach takes over
	}
	d.cur = c
	chunks, gap := d.ring.Since(h.LastSeq)
	if h.LastSeq > d.ring.Last() {
		// The client printed output from an earlier daemon of this name.
		chunks, _ = d.ring.Since(0)
		gap = true
	}
	first := d.ring.Last() + 1
	if len(chunks) > 0 {
		first = chunks[0].Seq
	}
	var flags uint8
	if gap {
		flags = proto.WelcomeGap
	}
	ok := d.writeLocked(c, proto.Frame{Type: proto.Welcome, Payload: proto.WelcomeMsg{Version: proto.Version, FirstSeq: first, Flags: flags}.Encode()})
	for _, ch := range chunks {
		if !ok {
			break
		}
		ok = d.writeLocked(c, proto.Frame{Type: proto.Output, Payload: proto.OutputMsg{Seq: ch.Seq, Data: ch.Data}.Encode()})
	}
	d.mu.Unlock()
	if !ok {
		return
	}

	d.setsize(h.Cols, h.Rows)
	if gap && h.Rows > 1 {
		// Nudge full-screen programs to repaint what the client lost.
		d.setsize(h.Cols, h.Rows-1)
		time.Sleep(50 * time.Millisecond)
		d.setsize(h.Cols, h.Rows)
	}

	defer func() {
		d.mu.Lock()
		if d.cur == c {
			d.cur = nil
		}
		d.mu.Unlock()
	}()
	for {
		f, err := proto.Read(c)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.Input:
			d.mu.Lock()
			p := d.ptmx
			d.mu.Unlock()
			if p != nil {
				if _, err := p.Write(f.Payload); err != nil {
					return
				}
			}
		case proto.Resize:
			if cols, rows, err := proto.DecodeResize(f.Payload); err == nil {
				d.setsize(cols, rows)
			}
		case proto.Ack:
			if seq, err := proto.DecodeAck(f.Payload); err == nil {
				d.ring.Trim(seq)
			}
		case proto.Ping:
			d.mu.Lock()
			ok := d.writeLocked(c, proto.Frame{Type: proto.Pong, Payload: f.Payload})
			d.mu.Unlock()
			if !ok {
				return
			}
		}
	}
}

// writeLocked sends f to c (d.mu held). On failure it drops c.
func (d *daemon) writeLocked(c net.Conn, f proto.Frame) bool {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := proto.Write(c, f); err != nil {
		c.Close()
		if d.cur == c {
			d.cur = nil
		}
		return false
	}
	return true
}

func (d *daemon) setsize(cols, rows uint16) {
	if cols == 0 || rows == 0 {
		return
	}
	d.mu.Lock()
	p := d.ptmx
	d.mu.Unlock()
	if p != nil {
		_ = pty.Setsize(p, &pty.Winsize{Cols: cols, Rows: rows})
	}
}

// ensureShell starts the login shell on the first HELLO, so it gets that
// client's TERM and size.
func (d *daemon) ensureShell(h proto.HelloMsg) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return nil
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	cmd.Env = shellEnv(os.Environ(), usableTerm(h.Term), d.session)
	cols, rows := h.Cols, h.Rows
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	d.ptmx = ptmx
	d.started = true
	readDone := make(chan struct{})
	go d.pump(ptmx, readDone)
	go func() {
		_ = cmd.Wait()
		// Let the last output drain; a background job still holding the
		// terminal must not keep the session alive.
		select {
		case <-readDone:
		case <-time.After(500 * time.Millisecond):
		}
		d.finish(exitCode(cmd.ProcessState))
	}()
	return nil
}

// pump reads the PTY into the ring and on to the attached client.
func (d *daemon) pump(ptmx *os.File, done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32<<10)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			d.mu.Lock()
			seq := d.ring.Append(buf[:n])
			if d.cur != nil {
				d.writeLocked(d.cur, proto.Frame{Type: proto.Output, Payload: proto.OutputMsg{Seq: seq, Data: buf[:n]}.Encode()})
			}
			d.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// finish tells the client the shell's status and takes the session down.
func (d *daemon) finish(code int) {
	d.mu.Lock()
	if d.cur != nil {
		d.writeLocked(d.cur, proto.Frame{Type: proto.Exit, Payload: proto.EncodeExit(code)})
		d.cur.Close()
		d.cur = nil
	}
	if d.ptmx != nil {
		d.ptmx.Close()
	}
	d.mu.Unlock()
	d.shutdown()
}

func (d *daemon) shutdown() {
	d.mu.Lock()
	if d.ln != nil {
		d.ln.Close() // removes the socket file
		d.ln = nil
	}
	d.mu.Unlock()
	os.Remove(d.path)
	select {
	case <-d.exited:
	default:
		close(d.exited)
	}
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
