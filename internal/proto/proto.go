// Package proto is the hqsh wire format: type (1 byte), length (4 bytes,
// big-endian), payload. See docs/protocol.md.
package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version a client and server must share.
const Version uint16 = 1

// MaxPayload bounds a frame so a corrupt length cannot exhaust memory.
const MaxPayload = 1 << 20

// Type identifies a frame.
type Type uint8

const (
	Hello   Type = 1
	Welcome Type = 2
	Output  Type = 3
	Input   Type = 4
	Resize  Type = 5
	Ack     Type = 6
	Ping    Type = 7
	Pong    Type = 8
	Exit    Type = 9
	// Status is local to the daemon socket and never crosses ssh: an empty
	// Status asks a daemon about itself; it answers with one Status frame
	// (flags u8, bit0: a client is attached; then, from 0.2.0, the number of
	// attached clients u16) and closes the connection.
	Status Type = 10
	// Detached tells a client that another client attached with --steal and
	// took the session over: stop, do not reconnect. Empty payload. (0.2.0+)
	Detached Type = 11
)

// Frame is one message.
type Frame struct {
	Type    Type
	Payload []byte
}

// ErrTooLarge is returned for a frame longer than MaxPayload.
var ErrTooLarge = errors.New("hqsh: frame too large")

// Write sends one frame.
func Write(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxPayload {
		return ErrTooLarge
	}
	buf := make([]byte, 5+len(f.Payload))
	buf[0] = byte(f.Type)
	binary.BigEndian.PutUint32(buf[1:], uint32(len(f.Payload)))
	copy(buf[5:], f.Payload)
	_, err := w.Write(buf)
	return err
}

// Read receives one frame.
func Read(r io.Reader) (Frame, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > MaxPayload {
		return Frame{}, ErrTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Type: Type(head[0]), Payload: payload}, nil
}

// HelloMsg opens (or resumes) a session.
type HelloMsg struct {
	Version uint16
	LastSeq uint64 // 0: nothing printed yet
	Cols    uint16
	Rows    uint16
	Session string
	Term    string // the client's $TERM; optional, after a 0 byte
	Flags   uint8  // HelloSteal, HelloReadOnly; optional, after a second 0 byte
}

// Hello flags (0.2.0+). With none set the attach is shared: every attached
// client gets all output and any client's input reaches the shell.
const (
	// HelloSteal detaches every other client (each gets Detached), like
	// `tmux attach -d`.
	HelloSteal uint8 = 1
	// HelloReadOnly attaches to watch: the daemon ignores this client's INPUT.
	HelloReadOnly uint8 = 2
)

// Encode a Hello payload: the fixed fields, the session name, then (only
// when Term or Flags is set) a 0 byte and Term, then (only when Flags is
// set) a 0 byte and the flags byte. A Hello without flags is byte for byte
// what a 0.1.x client sends.
func (h HelloMsg) Encode() []byte {
	b := make([]byte, 14, 17+len(h.Session)+len(h.Term))
	binary.BigEndian.PutUint16(b[0:], h.Version)
	binary.BigEndian.PutUint64(b[2:], h.LastSeq)
	binary.BigEndian.PutUint16(b[10:], h.Cols)
	binary.BigEndian.PutUint16(b[12:], h.Rows)
	b = append(b, h.Session...)
	if h.Term != "" || h.Flags != 0 {
		b = append(b, 0)
		b = append(b, h.Term...)
	}
	if h.Flags != 0 {
		b = append(b, 0, h.Flags)
	}
	return b
}

// DecodeHello parses a Hello payload. One without the 0 byte (an older
// client) has an empty Term; one without the second 0 byte has no Flags.
func DecodeHello(p []byte) (HelloMsg, error) {
	if len(p) < 14 {
		return HelloMsg{}, fmt.Errorf("hqsh: short hello (%d bytes)", len(p))
	}
	h := HelloMsg{
		Version: binary.BigEndian.Uint16(p[0:]),
		LastSeq: binary.BigEndian.Uint64(p[2:]),
		Cols:    binary.BigEndian.Uint16(p[10:]),
		Rows:    binary.BigEndian.Uint16(p[12:]),
	}
	rest := p[14:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		h.Session = string(rest)
		return h, nil
	}
	h.Session, rest = string(rest[:i]), rest[i+1:]
	if j := bytes.IndexByte(rest, 0); j >= 0 {
		h.Term = string(rest[:j])
		if len(rest) > j+1 {
			h.Flags = rest[j+1]
		}
	} else {
		h.Term = string(rest)
	}
	return h, nil
}

// WelcomeGap is the Welcome flag for "output you missed is gone; redraw".
const WelcomeGap uint8 = 1

// WelcomeShared is set by daemons that do shared attach (0.2.0+). A 0.1.x
// daemon leaves it clear: there, every attach takes over, and the Hello
// flags mean nothing.
const WelcomeShared uint8 = 2

// WelcomeMsg answers a Hello.
type WelcomeMsg struct {
	Version  uint16
	FirstSeq uint64 // the first seq the server will send
	Flags    uint8
}

// Gap reports the WelcomeGap flag.
func (w WelcomeMsg) Gap() bool { return w.Flags&WelcomeGap != 0 }

// Encode a Welcome payload.
func (w WelcomeMsg) Encode() []byte {
	b := make([]byte, 11)
	binary.BigEndian.PutUint16(b[0:], w.Version)
	binary.BigEndian.PutUint64(b[2:], w.FirstSeq)
	b[10] = w.Flags
	return b
}

// DecodeWelcome parses a Welcome payload.
func DecodeWelcome(p []byte) (WelcomeMsg, error) {
	if len(p) < 11 {
		return WelcomeMsg{}, fmt.Errorf("hqsh: short welcome (%d bytes)", len(p))
	}
	return WelcomeMsg{
		Version:  binary.BigEndian.Uint16(p[0:]),
		FirstSeq: binary.BigEndian.Uint64(p[2:]),
		Flags:    p[10],
	}, nil
}

// OutputMsg is a numbered chunk of terminal output.
type OutputMsg struct {
	Seq  uint64
	Data []byte
}

// Encode an Output payload.
func (o OutputMsg) Encode() []byte {
	b := make([]byte, 8, 8+len(o.Data))
	binary.BigEndian.PutUint64(b, o.Seq)
	return append(b, o.Data...)
}

// DecodeOutput parses an Output payload.
func DecodeOutput(p []byte) (OutputMsg, error) {
	if len(p) < 8 {
		return OutputMsg{}, fmt.Errorf("hqsh: short output (%d bytes)", len(p))
	}
	return OutputMsg{Seq: binary.BigEndian.Uint64(p), Data: p[8:]}, nil
}

// EncodeResize builds a Resize payload.
func EncodeResize(cols, rows uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return b
}

// DecodeResize parses a Resize payload.
func DecodeResize(p []byte) (cols, rows uint16, err error) {
	if len(p) < 4 {
		return 0, 0, fmt.Errorf("hqsh: short resize (%d bytes)", len(p))
	}
	return binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:]), nil
}

// EncodeAck builds an Ack payload (seq u64).
func EncodeAck(seq uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	return b
}

// EncodeStatus builds a Status reply: flags u8 (bit0: attached), clients u16.
func EncodeStatus(clients int) []byte {
	if clients > 0xffff {
		clients = 0xffff
	}
	b := make([]byte, 3)
	if clients > 0 {
		b[0] = 1
	}
	binary.BigEndian.PutUint16(b[1:], uint16(clients))
	return b
}

// DecodeStatus parses a Status reply. A 0.1.x daemon sends only the flags
// byte; its client count is then 1 when attached, else 0.
func DecodeStatus(p []byte) (attached bool, clients int, err error) {
	if len(p) < 1 {
		return false, 0, fmt.Errorf("hqsh: short status (%d bytes)", len(p))
	}
	attached = p[0]&1 != 0
	if len(p) >= 3 {
		clients = int(binary.BigEndian.Uint16(p[1:]))
	} else if attached {
		clients = 1
	}
	return attached, clients, nil
}

// DecodeAck parses an Ack payload.
func DecodeAck(p []byte) (uint64, error) {
	if len(p) < 8 {
		return 0, fmt.Errorf("hqsh: short ack (%d bytes)", len(p))
	}
	return binary.BigEndian.Uint64(p), nil
}

// EncodeExit builds an Exit payload (status i32).
func EncodeExit(code int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(code)))
	return b
}

// DecodeExit parses an Exit payload.
func DecodeExit(p []byte) (int, error) {
	if len(p) < 4 {
		return 0, fmt.Errorf("hqsh: short exit (%d bytes)", len(p))
	}
	return int(int32(binary.BigEndian.Uint32(p))), nil
}
