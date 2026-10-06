//go:build windows

package server

import (
	"errors"
	"io"
)

// ErrUnsupported: the server side needs a Unix PTY.
var ErrUnsupported = errors.New("hqsh: the server side runs on Linux and macOS only")

// Attach is Unix-only.
func Attach(session string) error { return ErrUnsupported }

// AttachIO is Unix-only.
func AttachIO(session string, in io.Reader, out io.Writer) error { return ErrUnsupported }

// Daemon is Unix-only.
func Daemon(session string) error { return ErrUnsupported }

// List is Unix-only.
func List() ([]SessionInfo, error) { return nil, ErrUnsupported }
