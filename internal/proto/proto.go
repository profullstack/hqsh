// Package proto is the hqsh wire format: type (1 byte), length (4 bytes,
// big-endian), payload. See docs/protocol.md.
package proto

import (
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
	var head [5]byte
	head[0] = byte(f.Type)
	binary.BigEndian.PutUint32(head[1:], uint32(len(f.Payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(f.Payload)
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
}

// Encode a Hello payload.
func (h HelloMsg) Encode() []byte {
	b := make([]byte, 14, 14+len(h.Session))
	binary.BigEndian.PutUint16(b[0:], h.Version)
	binary.BigEndian.PutUint64(b[2:], h.LastSeq)
	binary.BigEndian.PutUint16(b[10:], h.Cols)
	binary.BigEndian.PutUint16(b[12:], h.Rows)
	return append(b, h.Session...)
}

// DecodeHello parses a Hello payload.
func DecodeHello(p []byte) (HelloMsg, error) {
	if len(p) < 14 {
		return HelloMsg{}, fmt.Errorf("hqsh: short hello (%d bytes)", len(p))
	}
	return HelloMsg{
		Version: binary.BigEndian.Uint16(p[0:]),
		LastSeq: binary.BigEndian.Uint64(p[2:]),
		Cols:    binary.BigEndian.Uint16(p[10:]),
		Rows:    binary.BigEndian.Uint16(p[12:]),
		Session: string(p[14:]),
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
