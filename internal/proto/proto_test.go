package proto

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []Frame{
		{Type: Input, Payload: []byte("ls\r")},
		{Type: Output, Payload: OutputMsg{Seq: 42, Data: []byte("\x1b]1337;File=inline=1:AAAA\x07")}.Encode()},
		{Type: Ping, Payload: nil},
	}
	for _, f := range in {
		if err := Write(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range in {
		got, err := Read(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d: got %v %q, want %v %q", i, got.Type, got.Payload, want.Type, want.Payload)
		}
	}
}

func TestOversizedFramesAreRefused(t *testing.T) {
	if err := Write(&bytes.Buffer{}, Frame{Type: Output, Payload: make([]byte, MaxPayload+1)}); err != ErrTooLarge {
		t.Fatalf("write: %v", err)
	}
	// A corrupt length on the wire must not allocate it.
	if _, err := Read(bytes.NewReader([]byte{3, 0xff, 0xff, 0xff, 0xff})); err != ErrTooLarge {
		t.Fatalf("read: %v", err)
	}
}

func TestHelloAndOutputPayloads(t *testing.T) {
	h := HelloMsg{Version: Version, LastSeq: 7, Cols: 120, Rows: 40, Session: "main"}
	got, err := DecodeHello(h.Encode())
	if err != nil || got != h {
		t.Fatalf("hello: %+v %v", got, err)
	}
	o, err := DecodeOutput(OutputMsg{Seq: 9, Data: []byte("hi")}.Encode())
	if err != nil || o.Seq != 9 || string(o.Data) != "hi" {
		t.Fatalf("output: %+v %v", o, err)
	}
	c, r, err := DecodeResize(EncodeResize(200, 50))
	if err != nil || c != 200 || r != 50 {
		t.Fatalf("resize: %d %d %v", c, r, err)
	}
	if _, err := DecodeHello([]byte{1}); err == nil {
		t.Fatal("short hello accepted")
	}
}

func TestHelloCarriesTerm(t *testing.T) {
	h := HelloMsg{Version: Version, LastSeq: 3, Cols: 80, Rows: 24, Session: "work", Term: "xterm-kitty"}
	enc := h.Encode()
	if !bytes.HasSuffix(enc, []byte("work\x00xterm-kitty")) {
		t.Fatalf("encoding: %q", enc)
	}
	got, err := DecodeHello(enc)
	if err != nil || got != h {
		t.Fatalf("hello: %+v %v", got, err)
	}
}

func TestHelloWithoutTermStaysCompatible(t *testing.T) {
	// A 0.0.1 client sends no 0 byte; Term decodes empty and the session is intact.
	h := HelloMsg{Version: Version, Cols: 80, Rows: 24, Session: "main"}
	enc := h.Encode()
	if bytes.IndexByte(enc[14:], 0) >= 0 {
		t.Fatalf("empty Term must not add a separator: %q", enc)
	}
	got, err := DecodeHello(enc)
	if err != nil || got.Session != "main" || got.Term != "" {
		t.Fatalf("hello: %+v %v", got, err)
	}
}

func TestWelcomeAckExitPayloads(t *testing.T) {
	w := WelcomeMsg{Version: Version, FirstSeq: 12, Flags: WelcomeGap}
	got, err := DecodeWelcome(w.Encode())
	if err != nil || got != w || !got.Gap() {
		t.Fatalf("welcome: %+v %v", got, err)
	}
	if s, err := DecodeAck(EncodeAck(1 << 40)); err != nil || s != 1<<40 {
		t.Fatalf("ack: %d %v", s, err)
	}
	for _, code := range []int{0, 3, 130, -1} {
		if c, err := DecodeExit(EncodeExit(code)); err != nil || c != code {
			t.Fatalf("exit %d: %d %v", code, c, err)
		}
	}
}
