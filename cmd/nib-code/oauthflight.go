package main

import "sync"

// oauthFlight makes interactive sign-in idempotent: while a flow runs, a new
// connect reuses it instead of starting a second flow whose fresh state
// would invalidate every browser tab the first one opened.
type oauthFlight struct {
	mu      sync.Mutex
	active  bool
	authURL string
}

// begin claims the flight. ok is false when a flow is already running;
// authURL is then its sign-in URL, or "" before its browser step.
func (f *oauthFlight) begin() (authURL string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active {
		return f.authURL, false
	}
	f.active = true
	return "", true
}

// setURL records the running flow's sign-in URL for connects that join it.
func (f *oauthFlight) setURL(u string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authURL = u
}

// end releases the flight so the next connect starts a fresh flow.
func (f *oauthFlight) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active, f.authURL = false, ""
}
