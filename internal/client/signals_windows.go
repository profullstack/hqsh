//go:build windows

package client

import (
	"os"
	"os/signal"
	"time"
)

// watchSignals: Windows has no SIGWINCH, so poll the console size; an
// interrupt from the console quits cleanly. It returns a stop function.
func watchSignals(winch chan<- struct{}, quit func(), size func() (uint16, uint16)) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		lc, lr := size()
		for {
			select {
			case <-done:
				return
			case <-ch:
				quit()
			case <-t.C:
				if c, r := size(); c != lc || r != lr {
					lc, lr = c, r
					select {
					case winch <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
