package server

// Protocol-level test clients, shared by the Unix and Windows tests.

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/profullstack/hqsh/internal/proto"
)

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
	// until never matches before mark (the end of its last match) and has
	// already searched up to scanned.
	mark, scanned int
}

func dialPeer(t *testing.T, session string, lastSeq uint64) (*peer, proto.WelcomeMsg) {
	t.Helper()
	return dialHello(t, proto.HelloMsg{Version: proto.Version, LastSeq: lastSeq, Cols: 80, Rows: 24, Session: session, Term: "xterm-256color"})
}

func dialHello(t *testing.T, h proto.HelloMsg) (*peer, proto.WelcomeMsg) {
	t.Helper()
	session := h.Session
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

// until reads OUTPUT until the accumulated text contains want (searching
// only what arrived since the last match, so megabytes stay cheap).
func (p *peer) until(want string) {
	p.t.Helper()
	for {
		from := max(p.mark, p.scanned-len(want)+1)
		if i := strings.Index(p.out.String()[from:], want); i >= 0 {
			p.mark = from + i + len(want)
			p.scanned = p.mark
			return
		}
		p.scanned = p.out.Len()
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

// closed waits for the daemon to drop p, returning the frame types it got
// first.
func (p *peer) closed(within time.Duration) []proto.Type {
	p.t.Helper()
	var types []proto.Type
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-p.frames:
			if !ok {
				return types
			}
			types = append(types, f.Type)
		case <-deadline:
			p.t.Fatalf("not dropped within %v", within)
		}
	}
}

// detachedAndClosed: a --steal elsewhere sent p DETACHED and dropped it.
func (p *peer) detachedAndClosed() {
	p.t.Helper()
	types := p.closed(5 * time.Second)
	if len(types) == 0 || types[len(types)-1] != proto.Detached {
		p.t.Fatalf("frames before the drop %v, want DETACHED last", types)
	}
}

// clients is how many clients `server list` reports for name.
func clients(t *testing.T, name string) int {
	t.Helper()
	ss, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Name == name {
			return s.Clients
		}
	}
	return -1
}

func waitClients(t *testing.T, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := clients(t, name)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d clients listed, want %d", got, want)
		}
		time.Sleep(20 * time.Millisecond)
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
