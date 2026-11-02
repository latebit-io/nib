package main

import (
	"context"
	"sync"
	"time"
)

// oauthFlight serializes interactive sign-ins that bind a fixed callback
// port: a new connect cancels the in-flight one and waits for it to exit
// (releasing the port) before starting.
type oauthFlight struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// start cancels and awaits any previous flight, then returns a context for
// the new one plus a finish func the caller must invoke when it returns.
// Blocks while the previous flight unwinds; call off the UI goroutine.
func (f *oauthFlight) start(parent context.Context, timeout time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	done := make(chan struct{})

	f.mu.Lock()
	prevCancel, prevDone := f.cancel, f.done
	f.cancel, f.done = cancel, done
	f.mu.Unlock()

	if prevCancel != nil {
		prevCancel()
		<-prevDone
	}
	return ctx, func() {
		cancel()
		close(done)
	}
}
