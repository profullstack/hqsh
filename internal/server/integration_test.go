//go:build !windows

package server

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/profullstack/hqsh/internal/client"
	"github.com/profullstack/hqsh/internal/proto"
)

// The daemon runs as a separate process, as it does for real: attach
// re-execs this test binary, which TestMain turns into `hqsh server daemon`.
func TestMain(m *testing.M) {
	if os.Getenv("HQSH_TEST_DAEMON") == "1" {
		if err := Daemon(os.Getenv("HQSH_TEST_SESSION")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	daemonCommand = func(session string) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "HQSH_TEST_DAEMON=1", "HQSH_TEST_SESSION="+session)
		return cmd, nil
	}
	os.Exit(m.Run())
}

// sandbox gives the test its own socket dir (short, for macOS's 104-byte
// socket paths), home and a plain /bin/sh.
func sandbox(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hqsh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// End any session a failed test left running, then the dir.
		socks, _ := filepath.Glob(filepath.Join(dir, "hqsh", "*.sock"))
		for _, s := range socks {
			if c, err := net.Dial("unix", s); err == nil {
				h := proto.HelloMsg{Version: proto.Version, Cols: 80, Rows: 24, Session: "cleanup"}
				proto.Write(c, proto.Frame{Type: proto.Hello, Payload: h.Encode()})
				proto.Write(c, proto.Frame{Type: proto.Input, Payload: []byte("\nexit 0\n")})
				c.SetReadDeadline(time.Now().Add(3 * time.Second))
				io.Copy(io.Discard, c)
				c.Close()
			}
		}
		os.RemoveAll(dir)
	})
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	t.Setenv("PS1", "$ ")
}

// pipeConn is one end of an in-process attach: what ssh would carry.
type pipeConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	p.w.Close()
	p.r.Close()
	return nil
}

// attachPipe runs AttachIO the way `ssh host hqsh server attach` would.
func attachPipe(session string) *pipeConn {
	cr, sw := io.Pipe() // server -> client
	sr, cw := io.Pipe() // client -> server
	go func() {
		_ = AttachIO(session, sr, sw)
		sw.Close()
		sr.Close()
	}()
	return &pipeConn{r: cr, w: cw}
}

// peer is a protocol-level client.
type peer struct {
	t      *testing.T
	c      *pipeConn
	frames chan proto.Frame
	out    bytes.Buffer
	seqs   []uint64
}

func dialPeer(t *testing.T, session string, lastSeq uint64) (*peer, proto.WelcomeMsg) {
	t.Helper()
	p := &peer{t: t, c: attachPipe(session), frames: make(chan proto.Frame, 256)}
	go func() {
		defer close(p.frames)
		for {
			f, err := proto.Read(p.c)
			if err != nil {
				return
			}
			p.frames <- f
		}
	}()
	h := proto.HelloMsg{Version: proto.Version, LastSeq: lastSeq, Cols: 80, Rows: 24, Session: session, Term: "xterm-256color"}
	p.send(proto.Hello, h.Encode())
	f := p.next()
	if f.Type != proto.Welcome {
		t.Fatalf("first frame %d, want WELCOME", f.Type)
	}
	w, err := proto.DecodeWelcome(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return p, w
}

func (p *peer) send(typ proto.Type, payload []byte) {
	p.t.Helper()
	if err := proto.Write(p.c, proto.Frame{Type: typ, Payload: payload}); err != nil {
		p.t.Fatalf("send %d: %v", typ, err)
	}
}

func (p *peer) next() proto.Frame {
	p.t.Helper()
	select {
	case f, ok := <-p.frames:
		if !ok {
			p.t.Fatal("connection closed")
		}
		return f
	case <-time.After(10 * time.Second):
		p.t.Fatalf("timed out; output so far: %q", p.out.String())
	}
	return proto.Frame{}
}

// until reads OUTPUT until the accumulated text contains want.
func (p *peer) until(want string) {
	p.t.Helper()
	for !strings.Contains(p.out.String(), want) {
		f := p.next()
		if f.Type != proto.Output {
			continue
		}
		o, err := proto.DecodeOutput(f.Payload)
		if err != nil {
			p.t.Fatal(err)
		}
		p.seqs = append(p.seqs, o.Seq)
		p.out.Write(o.Data)
	}
}

func (p *peer) last() uint64 {
	if len(p.seqs) == 0 {
		return 0
	}
	return p.seqs[len(p.seqs)-1]
}

func contiguous(t *testing.T, seqs []uint64, from uint64) {
	t.Helper()
	for i, s := range seqs {
		if s != from+uint64(i) {
			t.Fatalf("seqs %v are not contiguous from %d", seqs, from)
		}
	}
}

func listed(t *testing.T, name string) (found, attached bool) {
	t.Helper()
	ss, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Name == name {
			return true, s.Attached
		}
	}
	return false, false
}

func TestDaemonResumesWithoutLossOrDuplicates(t *testing.T) {
	sandbox(t)
	const session = "t1"

	c1, w := dialPeer(t, session, 0)
	if w.Gap() {
		t.Fatal("fresh session reported a gap")
	}
	c1.until("$ ") // the shell is ready
	c1.send(proto.Input, []byte("echo hi\n"))
	c1.until("\nhi\r\n")
	if found, attached := listed(t, session); !found || !attached {
		t.Fatalf("list while attached: found=%v attached=%v", found, attached)
	}

	// Output that lands while nobody is attached.
	c1.send(proto.Input, []byte("sleep 1; echo la''ter\n"))
	c1.until("la''ter")
	contiguous(t, c1.seqs, 1)
	lastSeq := c1.last()
	c1.c.Close()

	time.Sleep(1500 * time.Millisecond)
	if found, attached := listed(t, session); !found || attached {
		t.Fatalf("list while detached: found=%v attached=%v", found, attached)
	}

	c2, w := dialPeer(t, session, lastSeq)
	if w.Gap() || w.FirstSeq != lastSeq+1 {
		t.Fatalf("resume welcome %+v, want no gap from %d", w, lastSeq+1)
	}
	c2.until("later\r\n")
	contiguous(t, c2.seqs, lastSeq+1)
	if strings.Contains(c2.out.String(), "la''ter") {
		t.Fatalf("replay repeated output the client already had: %q", c2.out.String())
	}
	all := c1.out.String() + c2.out.String()
	if n := strings.Count(all, "later\r\n"); n != 1 {
		t.Fatalf("missed output appears %d times in %q", n, all)
	}

	// A newer attach takes over; the older one is dropped.
	c3, _ := dialPeer(t, session, c2.last())
	select {
	case _, ok := <-c2.frames:
		for ok {
			_, ok = <-c2.frames
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the older attach was not dropped")
	}

	c3.send(proto.Input, []byte("exit 3\n"))
	for {
		f := c3.next()
		if f.Type == proto.Exit {
			code, _ := proto.DecodeExit(f.Payload)
			if code != 3 {
				t.Fatalf("exit status %d, want 3", code)
			}
			break
		}
	}
	path, _ := SocketPath(session)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket left behind after the shell exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestListRemovesStaleSockets(t *testing.T) {
	sandbox(t)
	dir, _ := SocketDir()
	if err := ensureDir(dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "gone.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ss, err := List()
	if err != nil || len(ss) != 0 {
		t.Fatalf("list: %+v %v", ss, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale socket not removed")
	}
}

// syncBuffer is a terminal stand-in the client writes to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q in %q", want, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The real client against the real daemon, with a pipe for ssh: it
// reconnects after the transport dies and prints the missed output once.
func TestClientReconnectsAndResumes(t *testing.T) {
	sandbox(t)
	const session = "t2"

	var mu sync.Mutex
	var conns []*pipeConn
	dial := func() (io.ReadWriteCloser, error) {
		c := attachPipe(session)
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
		return c, nil
	}
	keys, typed := io.Pipe()
	out := &syncBuffer{}
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := client.Run(client.Options{Host: "test", Session: session, Term: "xterm-256color",
			Stdin: keys, Stdout: out, Stderr: io.Discard, Dial: dial})
		done <- result{code, err}
	}()

	waitFor(t, out, "$ ")
	io.WriteString(typed, "echo hi\n")
	waitFor(t, out, "\nhi\r\n")
	io.WriteString(typed, "sleep 1; echo la''ter\n")
	waitFor(t, out, "la''ter")

	mu.Lock()
	conns[0].Close() // the network drops
	mu.Unlock()

	waitFor(t, out, "later\r\n")
	got := out.String()
	if !strings.Contains(got, "hqsh: reconnecting") {
		t.Fatalf("no reconnect status in the title: %q", got)
	}
	for _, s := range []string{"\nhi\r\n", "la''ter", "later\r\n"} {
		if n := strings.Count(got, s); n != 1 {
			t.Fatalf("%q printed %d times: %q", s, n, got)
		}
	}
	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d connections, want 2", n)
	}

	io.WriteString(typed, "exit 7\n")
	select {
	case r := <-done:
		if r.err != nil || r.code != 7 {
			t.Fatalf("Run = %d, %v; want 7", r.code, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client did not exit with the shell")
	}
}
