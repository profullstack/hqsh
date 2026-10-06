// Package client is the local side: puts your terminal in raw mode, runs
// `ssh host hqsh server attach <session>`, and keeps the session going across
// disconnects. See docs/protocol.md.
package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/profullstack/hqsh/internal/proto"
)

const (
	pingEvery   = 5 * time.Second
	deadAfter   = 15 * time.Second // no frame (PONG or anything) this long: reconnect
	ackEvery    = 64 << 10
	escapeByte  = 0x1e // Ctrl-^
	installHint = "curl -fsSL https://hqterm.sh/install | sh"
)

// Options for a connection.
type Options struct {
	Host    string   // [user@]host, as ssh takes it
	Session string   // default "main"
	SSHArgs []string // extra ssh flags (-p, -i, ...)
	Remote  string   // the hqsh binary on the host; default tries "hqsh", then ~/.local/bin/hqsh
	Term    string   // sent to the server as TERM; default $TERM

	// Steal detaches every other client of the session (tmux attach -d), on
	// the first connection only: a reconnect never kicks anyone.
	Steal bool
	// ReadOnly watches without typing: keys other than the detach escape
	// are dropped here, and the daemon ignores this client's INPUT anyway.
	ReadOnly bool

	// Tailscale is auto (default; $HQSH_TAILSCALE), on or off: whether to
	// reach the host over its tailnet address when it is an online peer.
	Tailscale string
	// Via and ViaKeyAlias, set by Run when the tailnet is used, point ssh at
	// the peer's tailnet address while keeping the host's known_hosts entry.
	Via, ViaKeyAlias string

	// Stdin, Stdout and Stderr default to the process's. Raw mode and window
	// size apply only when Stdin/Stdout are terminals.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Dial replaces ssh as the transport (tests, or a future WebSocket
	// bridge). It must reach `hqsh server attach <session>`.
	Dial func() (io.ReadWriteCloser, error)
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
	argv := []string{"ssh", "-T", "-o", "ServerAliveInterval=10"}
	if o.Via != "" {
		argv = append(argv, "-o", "HostName="+o.Via)
		if o.ViaKeyAlias != "" {
			argv = append(argv, "-o", "HostKeyAlias="+o.ViaKeyAlias)
		}
	}
	argv = append(argv, o.SSHArgs...)
	return append(argv, o.Host, remote, "server", "attach", session)
}

// NoRemoteError: the host has no hqsh to run.
type NoRemoteError struct{ Host string }

func (e *NoRemoteError) Error() string {
	return fmt.Sprintf("hqsh: %s has no hqsh (not on its PATH, nor in ~/.local/bin).\n"+
		"Install it on the host, then run hqsh again:\n\n    ssh %s '%s'\n", e.Host, e.Host, installHint)
}

// Run connects and stays connected until the session exits (returning the
// shell's exit status) or the user detaches with Ctrl-^ then '.' (returning 0).
// It fails only when the first connection cannot be made.
func Run(o Options) (int, error) {
	if o.Session == "" {
		o.Session = "main"
	}
	if o.Term == "" {
		o.Term = os.Getenv("TERM")
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	r := &runner{
		o:      o,
		input:  make(chan []byte, 16),
		winch:  make(chan struct{}, 1),
		quit:   make(chan struct{}),
		remote: o.Remote,
	}
	if o.Dial == nil {
		mode := o.Tailscale
		if mode == "" {
			mode = os.Getenv("HQSH_TAILSCALE")
		}
		if mode == "" {
			mode = TailscaleAuto
		}
		if mode != TailscaleAuto && mode != TailscaleOn && mode != TailscaleOff {
			return 2, fmt.Errorf("hqsh: --tailscale must be auto, on or off, not %q", mode)
		}
		addr, alias, err := tailnetRoute(o.Host, mode)
		if err != nil {
			return 1, err
		}
		r.tsAddr, r.tsAlias, r.tsRequired = addr, alias, mode == TailscaleOn
	}
	defer r.restoreTerminal()
	code, err := r.run()
	r.restoreTerminal()
	if err == errQuit {
		fmt.Fprintf(o.Stderr, "hqsh: detached; session %q keeps running on %s\n", o.Session, o.Host)
		return 0, nil
	}
	if err == errStolen {
		fmt.Fprintf(o.Stderr, "hqsh: detached by another client (--steal); session %q keeps running on %s\n", o.Session, o.Host)
		return 0, nil
	}
	return code, err
}

var (
	errQuit   = errors.New("quit")
	errStolen = errors.New("stolen")
)

type runner struct {
	o       Options
	lastSeq uint64 // the newest output printed
	remote  string

	stoleOnce bool // a WELCOME arrived, so --steal has been applied

	// The tailnet route, when the host is a Tailscale peer: tried first; a
	// first connection that fails over it falls back to the normal route, and
	// reconnects alternate between the two unless --tailscale on.
	tsAddr, tsAlias string
	tsRequired      bool
	tsBroken        bool // the first connection over the tailnet failed

	input    chan []byte
	winch    chan struct{}
	quit     chan struct{}
	quitOnce sync.Once

	rawOnce sync.Once
	restore func()
	stopSig func()
}

// outcome of one connection
type outcome int

const (
	lost outcome = iota
	exited
	quitting
	stolen // another client attached with --steal
)

func (r *runner) run() (int, error) {
	candidates := []string{r.o.Remote}
	if r.o.Remote == "" {
		candidates = []string{"hqsh", "~/.local/bin/hqsh"}
	}
	connected := false // got a WELCOME at least once
	inOutage := false
	attempt := 0
	for {
		r.o.Remote = candidates[0]
		r.route(connected, attempt)
		c, err := r.dial()
		if err != nil {
			if !connected {
				if r.o.Via != "" && !r.tsRequired {
					r.tsBroken = true // try the normal route
					continue
				}
				return 1, err
			}
		} else {
			res, code, welcomed := r.session(c, inOutage)
			if welcomed {
				connected, inOutage, attempt = true, false, 0
			}
			switch res {
			case exited:
				return code, nil
			case quitting:
				return 0, errQuit
			case stolen:
				return 0, errStolen
			}
			if !connected {
				status, stderr := exitStatus(c)
				if status == 127 && len(candidates) > 1 {
					candidates = candidates[1:]
					continue
				}
				if status == 127 {
					return 1, &NoRemoteError{Host: r.o.Host}
				}
				if r.o.Via != "" && !r.tsRequired {
					r.tsBroken = true // the tailnet path failed; try the normal route
					continue
				}
				msg := strings.TrimSpace(stderr)
				if msg == "" {
					msg = fmt.Sprintf("the connection closed (status %d)", status)
				}
				return 1, fmt.Errorf("hqsh: could not reach %s: %s", r.o.Host, msg)
			}
		}
		// Lost a live session: retry until it comes back or the user quits.
		if !inOutage {
			inOutage = true
			// Save the title, then show the status there, never on screen.
			io.WriteString(r.o.Stdout, "\x1b[22;0t\x1b]2;hqsh: reconnecting…\x07")
		}
		if r.sleep(Backoff(attempt)) {
			return 0, errQuit
		}
		attempt++
	}
}

// route picks the path for the next connection: the tailnet when there is
// one (always with --tailscale on), except after it failed the first
// connection; while reconnecting, every other attempt goes the normal way,
// so whichever network is up wins.
func (r *runner) route(connected bool, attempt int) {
	use := r.tsAddr != "" && (r.tsRequired || (!r.tsBroken && (!connected || attempt%2 == 0)))
	if use {
		r.o.Via, r.o.ViaKeyAlias = r.tsAddr, r.tsAlias
	} else {
		r.o.Via, r.o.ViaKeyAlias = "", ""
	}
}

// sleep waits d, dropping keystrokes typed while disconnected. True: quit.
func (r *runner) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return false
		case <-r.quit:
			return true
		case <-r.input:
		case <-r.winch:
		}
	}
}

func (r *runner) dial() (io.ReadWriteCloser, error) {
	if r.o.Dial != nil {
		return r.o.Dial()
	}
	return dialSSH(SSHCommand(r.o))
}

// session runs one connection until it ends.
func (r *runner) session(c io.ReadWriteCloser, afterOutage bool) (res outcome, code int, welcomed bool) {
	var wmu sync.Mutex
	send := func(f proto.Frame) error {
		wmu.Lock()
		defer wmu.Unlock()
		return proto.Write(c, f)
	}
	var heard atomic.Int64
	heard.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	defer func() {
		close(stop)
		c.Close()
	}()

	// Watchdog: ping, and cut a connection that has gone quiet. Closing it
	// unblocks any read or write stuck on it.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		lastPing := time.Now()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				if now.Sub(time.Unix(0, heard.Load())) > deadAfter {
					c.Close()
					return
				}
				if now.Sub(lastPing) >= pingEvery && wmu.TryLock() {
					_ = proto.Write(c, proto.Frame{Type: proto.Ping, Payload: make([]byte, 8)})
					wmu.Unlock()
					lastPing = now
				}
			}
		}
	}()

	cols, rows := r.size()
	hello := proto.HelloMsg{Version: proto.Version, LastSeq: r.lastSeq, Cols: cols, Rows: rows, Session: r.o.Session, Term: r.o.Term}
	if r.o.ReadOnly {
		hello.Flags |= proto.HelloReadOnly
	}
	if r.o.Steal && !r.stoleOnce {
		hello.Flags |= proto.HelloSteal
	}
	if err := send(proto.Frame{Type: proto.Hello, Payload: hello.Encode()}); err != nil {
		return lost, 0, false
	}

	frames := make(chan proto.Frame, 64)
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := proto.Read(c)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-stop:
				return
			}
		}
	}()

	unacked := 0
	// handle one frame; done reports the connection is over.
	handle := func(f proto.Frame) (res outcome, code int, done bool) {
		heard.Store(time.Now().UnixNano())
		switch f.Type {
		case proto.Welcome:
			w, err := proto.DecodeWelcome(f.Payload)
			if err != nil {
				return lost, 0, true
			}
			welcomed = true
			r.stoleOnce = true // reconnects never steal again
			if afterOutage {
				io.WriteString(r.o.Stdout, "\x1b[23;0t") // restore the saved title
			}
			if w.Gap() {
				// What we missed is gone: start clean from what the server has.
				io.WriteString(r.o.Stdout, "\x1b[2J\x1b[H")
				if w.FirstSeq > 0 {
					r.lastSeq = w.FirstSeq - 1
				}
			}
			r.startInteractive()
		case proto.Output:
			o, err := proto.DecodeOutput(f.Payload)
			if err != nil {
				return lost, 0, true
			}
			if o.Seq <= r.lastSeq {
				return 0, 0, false // already printed
			}
			if _, err := r.o.Stdout.Write(o.Data); err != nil {
				return quitting, 0, true
			}
			r.lastSeq = o.Seq
			unacked += len(o.Data)
			if unacked >= ackEvery {
				unacked = 0
				if send(proto.Frame{Type: proto.Ack, Payload: proto.EncodeAck(r.lastSeq)}) != nil {
					return lost, 0, true
				}
			}
		case proto.Exit:
			code, _ := proto.DecodeExit(f.Payload)
			return exited, code, true
		case proto.Detached:
			return stolen, 0, true
		}
		return 0, 0, false
	}
	for {
		select {
		case f := <-frames:
			if res, code, done := handle(f); done {
				return res, code, welcomed
			}
		case <-readErr:
			// Finish what arrived before the error (e.g. the last output and EXIT).
			for {
				select {
				case f := <-frames:
					if res, code, done := handle(f); done {
						return res, code, welcomed
					}
				default:
					return lost, 0, welcomed
				}
			}
		case b := <-r.input:
			if r.o.ReadOnly {
				continue // watching only
			}
			if send(proto.Frame{Type: proto.Input, Payload: b}) != nil {
				return lost, 0, welcomed
			}
		case <-r.winch:
			cols, rows := r.size()
			if send(proto.Frame{Type: proto.Resize, Payload: proto.EncodeResize(cols, rows)}) != nil {
				return lost, 0, welcomed
			}
		case <-r.quit:
			return quitting, 0, welcomed
		}
	}
}

// startInteractive puts the terminal in raw mode and starts reading keys,
// once, after the first WELCOME (so ssh can still prompt before that).
func (r *runner) startInteractive() {
	r.rawOnce.Do(func() {
		if f, ok := r.o.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			if st, err := term.MakeRaw(int(f.Fd())); err == nil {
				fd := int(f.Fd())
				r.restore = func() { _ = term.Restore(fd, st) }
			}
		}
		r.stopSig = watchSignals(r.winch, r.doQuit, r.size)
		go r.readInput()
	})
}

func (r *runner) restoreTerminal() {
	if r.stopSig != nil {
		r.stopSig()
		r.stopSig = nil
	}
	if r.restore != nil {
		r.restore()
		r.restore = nil
	}
}

func (r *runner) doQuit() { r.quitOnce.Do(func() { close(r.quit) }) }

// readInput turns keystrokes into INPUT, handling the escape: Ctrl-^ '.'
// detaches, Ctrl-^ Ctrl-^ sends one Ctrl-^, Ctrl-^ anything-else sends both.
func (r *runner) readInput() {
	buf := make([]byte, 4096)
	esc := false
	for {
		n, err := r.o.Stdin.Read(buf)
		if n > 0 {
			out := make([]byte, 0, n+1)
			for _, b := range buf[:n] {
				switch {
				case esc && b == '.':
					r.doQuit()
					return
				case esc && b == escapeByte:
					esc = false
					out = append(out, escapeByte)
				case esc:
					esc = false
					out = append(out, escapeByte, b)
				case b == escapeByte:
					esc = true
				default:
					out = append(out, b)
				}
			}
			if len(out) > 0 {
				select {
				case r.input <- out:
				case <-r.quit:
					return
				}
			}
		}
		if err != nil {
			r.doQuit() // stdin closed: nothing more to type
			return
		}
	}
}

func (r *runner) size() (cols, rows uint16) {
	if f, ok := r.o.Stdout.(*os.File); ok {
		if w, h, err := term.GetSize(int(f.Fd())); err == nil && w > 0 && h > 0 {
			return uint16(w), uint16(h)
		}
	}
	return 80, 24
}

// sshConn is ssh's stdin and stdout as one stream.
type sshConn struct {
	cmd    *exec.Cmd
	in     *os.File // our end of ssh's stdin
	out    *os.File // our end of ssh's stdout
	stderr *tail
	done   chan struct{}
	status int
	once   sync.Once
}

func dialSSH(argv []string) (*sshConn, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	s := &sshConn{in: inW, out: outR, stderr: &tail{max: 4096}, done: make(chan struct{})}
	s.cmd = exec.Command(argv[0], argv[1:]...)
	// Real files, not Go-managed pipes, so Wait never closes our read end
	// before we have read ssh's last bytes (the EXIT frame).
	s.cmd.Stdin, s.cmd.Stdout, s.cmd.Stderr = inR, outW, s.stderr
	err = s.cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, fmt.Errorf("hqsh: running ssh: %w", err)
	}
	go func() {
		_ = s.cmd.Wait()
		s.status = s.cmd.ProcessState.ExitCode()
		close(s.done)
	}()
	return s, nil
}

func (s *sshConn) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *sshConn) Write(p []byte) (int, error) { return s.in.Write(p) }

// Close ends ssh: closing its stdin lets it exit on its own; a hung one is
// killed after 2s.
func (s *sshConn) Close() error {
	s.once.Do(func() {
		s.in.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
		s.out.Close()
	})
	return nil
}

// exitStatus is the remote command's status and ssh's stderr, when c is ssh.
func exitStatus(c io.ReadWriteCloser) (int, string) {
	s, ok := c.(*sshConn)
	if !ok {
		return -1, ""
	}
	s.Close()
	return s.status, s.stderr.String()
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
