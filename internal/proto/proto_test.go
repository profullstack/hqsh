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
