package main

import "testing"

func TestOAuthFlight_JoinsRunningFlow(t *testing.T) {
	var f oauthFlight
	if _, ok := f.begin(); !ok {
		t.Fatal("first begin did not claim the flight")
	}
	if u, ok := f.begin(); ok || u != "" {
		t.Fatalf("begin during flow = (%q, %v); want (\"\", false) before the browser step", u, ok)
	}
	f.setURL("https://auth.example/authorize?state=s1")
	if u, ok := f.begin(); ok || u != "https://auth.example/authorize?state=s1" {
		t.Fatalf("begin during flow = (%q, %v); want the running flow's URL", u, ok)
	}

	f.end()
	if u, ok := f.begin(); !ok || u != "" {
		t.Errorf("begin after end = (%q, %v); want a fresh claim", u, ok)
	}
}
