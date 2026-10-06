package server

import "io"

// console is the shell's terminal as the daemon sees it: a PTY on Unix, a
// pseudo console (ConPTY) on Windows. Reads are the shell's output, writes
// are keystrokes.
type console interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
}

// The platform files provide:
//
//	startShell(session, term string, cols, rows uint16) (console, func() int, error)
//	    starts the user's shell on a new console; the func waits for it to
//	    exit and returns its status (128+signal for a signal on Unix).
//	lockSession(f *os.File) error   exclusive, non-blocking: one daemon per session
//	chmodSocket(path string) error  the socket is the user's alone
//	isDead(err error) bool          a dial error meaning no daemon is behind the socket
