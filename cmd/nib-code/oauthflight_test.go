package main

import (
	"context"
	"testing"
	"time"
)

func TestOAuthFlight_NewStartCancelsAndAwaitsPrevious(t *testing.T) {
	var f oauthFlight
	ctx1, finish1 := f.start(context.Background(), time.Minute)

	type flight struct {
		ctx    context.Context
		finish func()
	}
	second := make(chan flight, 1)
	go func() {
		ctx2, finish2 := f.start(context.Background(), time.Minute)
		second <- flight{ctx2, finish2}
	}()

	select {
	case <-ctx1.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("previous flight was not cancelled")
	}
	// The previous flight still holds the callback port until it finishes.
	select {
	case <-second:
		t.Fatal("new flight started before the previous one finished")
	case <-time.After(50 * time.Millisecond):
	}

	finish1()
	select {
	case fl := <-second:
		defer fl.finish()
		if fl.ctx.Err() != nil {
			t.Errorf("new flight context already done: %v", fl.ctx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new flight never started after the previous one finished")
	}
}

func TestOAuthFlight_StartAfterFinishDoesNotBlock(t *testing.T) {
	var f oauthFlight
	_, finish := f.start(context.Background(), time.Minute)
	finish()

	done := make(chan struct{})
	go func() {
		_, finish := f.start(context.Background(), time.Minute)
		finish()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("start blocked on an already-finished flight")
	}
}
