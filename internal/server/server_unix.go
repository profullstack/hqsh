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
	helloTimeout = 30 * time.Second // a daemon nobody says HELLO to gives up
	socketCheck  = 5 * time.Second  // how often the daemon checks its socket file still exists
)

// writeTimeout: a client that cannot take one frame this long is dropped (it
// reconnects and resumes). A var so tests can shorten it.
var writeTimeout = 5 * time.Second

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
// closes: the client went away, or the daemon dropped this connection
// (another client attached with --steal, the client fell too far behind, or
// the shell exited).
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
		attached, clients, err := status(p)
		if err != nil {
			if isDead(err) {
				os.Remove(p)
			}
			continue
		}
		out = append(out, SessionInfo{Name: name, Attached: attached, Clients: clients})
	}
	return out, nil
}

func isDead(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// status asks a daemon whether clients are attached, and how many.
func status(path string) (attached bool, clients int, err error) {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false, 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if err := proto.Write(c, proto.Frame{Type: proto.Status}); err != nil {
		return false, 0, err
	}
	f, err := proto.Read(c)
	if err != nil {
		return false, 0, err
	}
	if f.Type != proto.Status {
		return false, 0, fmt.Errorf("hqsh: unexpected status reply")
	}
	return proto.DecodeStatus(f.Payload)
}

// daemon is one session: a PTY, its output ring, and the clients attached
// to it (any number, tmux style: all of them see the output, and any of
// them that is not read-only can type).
type daemon struct {
	session string
	path    string
	ring    *ring.Buffer

	mu         sync.Mutex // guards everything below
	clients    map[*member]struct{}
	ptmx       *os.File
	started    bool
	ln         net.Listener
	cols, rows uint16 // the PTY's size: the smallest among the clients
	exiting    bool   // the shell is gone; writers send EXIT once drained
	exitCode   int
	writers    sync.WaitGroup // one per attached client
	progress   *sync.Cond     // on d.mu: a client took output, left, or the shell ended

	exited chan struct{} // closed once the shell is gone and EXIT went out
}

// member is one attached connection. Its send queue is its cursor into the
// shared ring (sent): output is written by its own goroutine, so a slow
// client never stalls the PTY or the other clients. The queue is bounded by
// the ring's budget: a client whose cursor falls off the ring, or that
// cannot take a frame within writeTimeout, is dropped and comes back
// through the resume (gap) path.
type member struct {
	c        net.Conn
	readOnly bool
	size     winsize // d.mu
	sent     uint64  // newest seq written to it (d.mu)
	acked    uint64  // newest seq it acknowledged, <= sent (d.mu)

	wake chan struct{} // cap 1: there is new output, or the shell ended
	done chan struct{} // closed when the client is dropped
	once sync.Once
	wmu  sync.Mutex // one frame at a time on c
}

func newMember(c net.Conn, h proto.HelloMsg) *member {
	return &member{
		c:        c,
		readOnly: h.Flags&proto.HelloReadOnly != 0,
		size:     winsize{h.Cols, h.Rows},
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// write sends one frame, giving up after writeTimeout.
func (cl *member) write(f proto.Frame) error {
	cl.wmu.Lock()
	defer cl.wmu.Unlock()
	cl.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return proto.Write(cl.c, f)
}

func (cl *member) poke() {
	select {
	case cl.wake <- struct{}{}:
	default:
	}
}

func (cl *member) close() {
	cl.once.Do(func() {
		close(cl.done)
		cl.c.Close()
	})
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
	d := &daemon{session: session, path: path, ring: ring.New(DefaultBuffer),
		clients: map[*member]struct{}{}, exited: make(chan struct{})}
	d.progress = sync.NewCond(&d.mu)
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
		n := len(d.clients)
		d.mu.Unlock()
		_ = proto.Write(c, proto.Frame{Type: proto.Status, Payload: proto.EncodeStatus(n)})
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

	cl := newMember(c, h)
	d.mu.Lock()
	if d.exiting {
		code := d.exitCode
		d.mu.Unlock()
		_ = cl.write(proto.Frame{Type: proto.Exit, Payload: proto.EncodeExit(code)})
		return
	}
	if h.Flags&proto.HelloSteal != 0 {
		// tmux attach -d: everyone else is told and let go.
		for o := range d.clients {
			delete(d.clients, o)
			go o.detach()
		}
		d.progress.Broadcast()
	}
	// Where this client's output starts: right after what it printed, or,
	// when that is no longer buffered (or it printed output from an earlier
	// daemon of this name), the oldest output still buffered.
	from := h.LastSeq
	first := d.ring.First()
	gap := from > d.ring.Last() || from+1 < first
	if gap {
		from = first - 1
	}
	cl.sent, cl.acked = from, from
	d.clients[cl] = struct{}{}
	d.resizeLocked()
	d.writers.Add(1)
	d.mu.Unlock()
	defer d.drop(cl)

	flags := proto.WelcomeShared
	if gap {
		flags |= proto.WelcomeGap
	}
	go d.writer(cl, proto.Frame{Type: proto.Welcome, Payload: proto.WelcomeMsg{Version: proto.Version, FirstSeq: from + 1, Flags: flags}.Encode()})
	if gap {
		d.nudge() // full-screen programs repaint what the client lost
	}

	for {
		f, err := proto.Read(c)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.Input:
			if cl.readOnly {
				continue // watching only
			}
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
				d.mu.Lock()
				cl.size = winsize{cols, rows}
				d.resizeLocked()
				d.mu.Unlock()
			}
		case proto.Ack:
			if seq, err := proto.DecodeAck(f.Payload); err == nil {
				d.ack(cl, seq)
			}
		case proto.Ping:
			if cl.write(proto.Frame{Type: proto.Pong, Payload: f.Payload}) != nil {
				return
			}
		}
	}
}

// writer sends cl its WELCOME, then every chunk after its cursor, oldest
// first, then waits for more. It drops cl when a write fails or times out,
// or when cl's cursor fell off the ring (the client reconnects and gets the
// gap path). Once the shell is gone and cl is drained it sends EXIT.
func (d *daemon) writer(cl *member, welcome proto.Frame) {
	defer d.writers.Done()
	defer d.drop(cl)
	if cl.write(welcome) != nil {
		return
	}
	for {
		d.mu.Lock()
		from, exiting, code := cl.sent, d.exiting, d.exitCode
		d.mu.Unlock()
		chunks, gap := d.ring.Since(from)
		if gap {
			return // fell behind past the buffer
		}
		for _, ch := range chunks {
			if cl.write(proto.Frame{Type: proto.Output, Payload: proto.OutputMsg{Seq: ch.Seq, Data: ch.Data}.Encode()}) != nil {
				return
			}
			d.mu.Lock()
			cl.sent = ch.Seq
			d.progress.Broadcast()
			d.mu.Unlock()
		}
		if len(chunks) > 0 {
			continue
		}
		if exiting {
			_ = cl.write(proto.Frame{Type: proto.Exit, Payload: proto.EncodeExit(code)})
			return
		}
		select {
		case <-cl.wake:
		case <-cl.done:
			return
		}
	}
}

// ack records what cl has printed and trims the ring to what every
// attached client has.
func (d *daemon) ack(cl *member, seq uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if seq > cl.sent {
		seq = cl.sent // it cannot have printed what it was never sent
	}
	if seq > cl.acked {
		cl.acked = seq
	}
	acks := make([]uint64, 0, len(d.clients))
	for o := range d.clients {
		acks = append(acks, o.acked)
	}
	if upTo, ok := minAck(acks); ok {
		d.ring.Trim(upTo) // under d.mu, so no client attaches mid-trim
	}
}

// drop detaches cl (any number of times) and refits the PTY to the clients
// left.
func (d *daemon) drop(cl *member) {
	d.mu.Lock()
	if _, ok := d.clients[cl]; ok {
		delete(d.clients, cl)
		d.resizeLocked()
		d.progress.Broadcast()
	}
	d.mu.Unlock()
	cl.close()
}

// detach tells a client another one took the session (--steal), then lets
// it go. The caller already removed it from d.clients.
func (cl *member) detach() {
	_ = cl.write(proto.Frame{Type: proto.Detached})
	cl.close()
}

// resizeLocked sets the PTY to the smallest size among the attached
// clients, when that changed (d.mu held).
func (d *daemon) resizeLocked() {
	sizes := make([]winsize, 0, len(d.clients))
	for o := range d.clients {
		sizes = append(sizes, o.size)
	}
	cols, rows, ok := minSize(sizes)
	if !ok || (cols == d.cols && rows == d.rows) {
		return
	}
	d.cols, d.rows = cols, rows
	if d.ptmx != nil {
		_ = pty.Setsize(d.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	}
}

// nudge makes full-screen programs repaint: one row less, then back.
func (d *daemon) nudge() {
	d.mu.Lock()
	p, cols, rows := d.ptmx, d.cols, d.rows
	d.mu.Unlock()
	if p == nil || rows < 2 {
		return
	}
	_ = pty.Setsize(p, &pty.Winsize{Cols: cols, Rows: rows - 1})
	time.Sleep(50 * time.Millisecond)
	d.mu.Lock()
	_ = pty.Setsize(p, &pty.Winsize{Cols: d.cols, Rows: d.rows})
	d.mu.Unlock()
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
	d.cols, d.rows = cols, rows
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

// pump reads the PTY into the ring and wakes every client's writer. It
// never waits on one particular client: it pauses only while even the most
// caught-up client is more than paceWindow behind (see mustWait).
func (d *daemon) pump(ptmx *os.File, done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32<<10)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			d.ring.Append(buf[:n])
			d.mu.Lock()
			for cl := range d.clients {
				cl.poke()
			}
			for !d.exiting && mustWait(d.backlogsLocked(), paceWindow) {
				d.progress.Wait()
			}
			d.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// backlogsLocked is each attached client's unsent output in bytes (d.mu held).
func (d *daemon) backlogsLocked() []int {
	out := make([]int, 0, len(d.clients))
	for cl := range d.clients {
		out = append(out, d.ring.Behind(cl.sent))
	}
	return out
}

// finish lets every client drain its output and get the shell's status,
// then takes the session down.
func (d *daemon) finish(code int) {
	d.mu.Lock()
	d.exiting, d.exitCode = true, code
	for cl := range d.clients {
		cl.poke()
	}
	d.progress.Broadcast()
	d.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		d.writers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(writeTimeout + time.Second):
	}
	d.mu.Lock()
	for cl := range d.clients {
		cl.close()
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
