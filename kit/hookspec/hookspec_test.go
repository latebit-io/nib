package hookspec

import (
	"slices"
	"testing"
)

const sample = `{
  "hooks": {
    "PreToolUse": [
      { "matcher": "Write|Edit", "hooks": [ { "type": "command", "command": "fmt.sh", "env": {"X":"1"}, "timeout": 5000 } ] }
    ],
    "PostToolUse": [
      { "matcher": "", "hooks": [ { "type": "command", "command": ["prettier", "--write"] } ] }
    ],
    "SessionStart": [
      { "hooks": [ { "type": "http", "url": "https://x/hook" } ] }
    ],
    "WeirdEvent": [
      { "hooks": [ { "type": "command", "command": "x.sh" } ] }
    ]
  }
}`

func TestParse_StructureAndNormalization(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	pre := cfg.Hooks[PreToolUse]
	if len(pre) != 1 || len(pre[0].Hooks) != 1 {
		t.Fatalf("PreToolUse shape: %+v", pre)
	}
	h := pre[0].Hooks[0]
	if h.Type != Command || !slices.Equal(h.Command.Parts, []string{"fmt.sh"}) || !h.Command.Shell {
		t.Errorf("command string form not preserved: %+v", h)
	}
	if h.Env["X"] != "1" || h.TimeoutMS != 5000 {
		t.Errorf("env/timeout: %+v", h)
	}
	if !h.Runnable() {
		t.Errorf("command hook should be runnable")
	}

	// Array command keeps argv form (Shell false).
	post := cfg.Hooks[PostToolUse][0].Hooks[0]
	if !slices.Equal(post.Command.Parts, []string{"prettier", "--write"}) || post.Command.Shell {
		t.Errorf("array command: %+v", post.Command)
	}

	// http hook parses but is not runnable.
	ss := cfg.Hooks[SessionStart][0].Hooks[0]
	if ss.Type != HTTP || ss.URL != "https://x/hook" || ss.Runnable() {
		t.Errorf("http hook: %+v", ss)
	}
}

func TestGroup_Matches(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	pre := cfg.Hooks[PreToolUse][0]
	if !pre.Matches("Write") || !pre.Matches("Edit") {
		t.Errorf("Write|Edit should match Write and Edit")
	}
	if pre.Matches("Read") {
		t.Errorf("Read should not match Write|Edit")
	}
	// Empty matcher matches everything.
	if !cfg.Hooks[PostToolUse][0].Matches("anything") {
		t.Errorf("empty matcher should match all")
	}
}

func TestParse_Unsupported(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	un := cfg.Unsupported()
	// http hook (SessionStart) + unknown event (WeirdEvent) are reported;
	// the WeirdEvent's command hook is also flagged via its event.
	hasHTTP := slices.ContainsFunc(un, func(u Unsupported) bool {
		return u.Event == SessionStart && u.Reason == `"http" hook type not yet runnable`
	})
	hasUnknown := slices.ContainsFunc(un, func(u Unsupported) bool {
		return u.Event == "WeirdEvent" && u.Reason == "unrecognized event"
	})
	if !hasHTTP || !hasUnknown {
		t.Errorf("unsupported = %+v", un)
	}
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()
	// Object matcher (CC's {if:...} form) is rejected.
	if _, err := Parse([]byte(`{"hooks":{"PreToolUse":[{"matcher":{"if":["Write"]},"hooks":[]}]}}`)); err == nil {
		t.Errorf("object matcher should be rejected")
	}
	// Invalid regex matcher.
	if _, err := Parse([]byte(`{"hooks":{"PreToolUse":[{"matcher":"(unclosed","hooks":[]}]}}`)); err == nil {
		t.Errorf("invalid regex matcher should error")
	}
	// Malformed JSON.
	if _, err := Parse([]byte(`{not json`)); err == nil {
		t.Errorf("malformed json should error")
	}
}

func TestCommandSpec_FormRoundTrips(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"hooks":{"PreToolUse":[
	  {"hooks":[{"type":"command","command":"a | b"}]},
	  {"hooks":[{"type":"command","command":["c","--flag"]}]}
	]}}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Re-parsing the managed copy must preserve shell vs argv form.
	rt, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	g := rt.Hooks[PreToolUse]
	if !g[0].Hooks[0].Command.Shell {
		t.Errorf("shell command form lost on round-trip")
	}
	if g[1].Hooks[0].Command.Shell {
		t.Errorf("argv command form became shell on round-trip")
	}
}

func TestParse_RejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	if _, err := Parse([]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command"}]}]}}`)); err == nil {
		t.Errorf("command hook with no command should be rejected")
	}
	if _, err := Parse([]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":[]}]}]}}`)); err == nil {
		t.Errorf("command hook with empty array should be rejected")
	}
	// An empty-string command unmarshals to a single blank part; reject it too.
	if _, err := Parse([]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":""}]}]}}`)); err == nil {
		t.Errorf("command hook with empty string should be rejected")
	}
}

func TestEvent_Known(t *testing.T) {
	t.Parallel()
	if !PreToolUse.Known() || !Stop.Known() {
		t.Errorf("known events should report Known()")
	}
	if Event("Nope").Known() {
		t.Errorf("unknown event should not be Known()")
	}
}
