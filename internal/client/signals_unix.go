//go:build !windows

package client

import (
	"os"
	"os/signal"
	"syscall"
)

// watchSignals turns SIGWINCH into a resize nudge and SIGTERM/SIGHUP into a
// clean quit (so the terminal is restored). It returns a stop function.
func watchSignals(winch chan<- struct{}, quit func(), _ func() (uint16, uint16)) func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case s := <-ch:
				if s == syscall.SIGWINCH {
					select {
					case winch <- struct{}{}:
					default:
					}
					continue
				}
				quit()
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
