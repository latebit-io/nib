package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ErrUnknownCommand is the sentinel returned (wrapped in
// [CommandError]) when input parses as a slash command but no
// matching name is registered.
var ErrUnknownCommand = errors.New("unknown command")

// ErrTurnInFlight is the sentinel returned (wrapped in
// [CommandError]) when [Registry.Dispatch] is called with a
// [WithBusyCheck] option whose probe reports an in-flight agent
// turn. Phase 1 dispatches between turns only; mid-stream
// dispatch is a future capability.
var ErrTurnInFlight = errors.New("turn in flight")

// ErrInvalidCommandShape is the sentinel returned (wrapped in
// [CommandError]) when a registered Command implements neither
// [HandlerCommand] nor [PromptCommand]. Adding a new shape (e.g.
// a streaming command) requires extending [Registry.Dispatch];
// this sentinel guards against silent misregistration.
var ErrInvalidCommandShape = errors.New("invalid command shape")

// ErrCollision is the sentinel returned by [Registry.Register]
// when a command is registered with the same name and source kind
// as an existing entry. Cross-kind shadowing is silent; same-kind
// collision is a programmer error.
var ErrCollision = errors.New("registration collision")

// CommandError wraps a sentinel or handler error with the command
// name that produced it. Frontends use [errors.Is] against the
// sentinels above to render appropriately.
type CommandError struct {
	// Name is the canonical command name (lowercase, no slash). For
	// unknown commands it is the name the user typed; for handler
	// errors it is the registered command's canonical name.
	Name string

	// Err is the underlying cause — a sentinel from this package or
	// an error returned by the command's Handle/Render method.
	Err error
}

// Error formats the wrapped error with the command name prefix.
func (e *CommandError) Error() string {
	if e == nil {
		return "<nil command error>"
	}
	return fmt.Sprintf("command %q: %v", e.Name, e.Err)
}

// Unwrap exposes the underlying cause for [errors.Is] / [errors.As].
func (e *CommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// dispatchOpts carries the optional behavior flags for one
// [Registry.Dispatch] call. Constructed only inside Dispatch.
type dispatchOpts struct {
	busy func() bool
}

// DispatchOption tunes one [Registry.Dispatch] call. Use
// [WithBusyCheck] to refuse handler dispatch while the agent has a
// turn in flight.
type DispatchOption func(*dispatchOpts)

// WithBusyCheck installs a probe that returns true when an agent
// turn is in flight. Dispatch returns [ErrTurnInFlight] (wrapped
// in [CommandError]) when busy reports true. The probe is called
// once per Dispatch, before lookup runs the command.
//
// PromptCommand dispatch routes through SubmitPrompt and is
// safe to call between turns by definition; the busy check still
// applies because submitting a prompt while a turn is in flight
// would race the agent's input handling.
func WithBusyCheck(busy func() bool) DispatchOption {
	return func(o *dispatchOpts) {
		o.busy = busy
	}
}

// precedenceOrder maps each [SourceKind] to its shadowing rank.
// Higher rank wins on cross-kind collision at registration time.
// Same-rank collision returns [ErrCollision].
//
// Project commands trump everything (the user explicitly authored
// them inside the repo). Global markdown trumps MCP and builtins
// (the user installed them under their config directory). MCP
// trumps builtins (the user installed the MCP server). Builtins
// are the floor.
var precedenceOrder = map[SourceKind]int{
	SourceBuiltin: 0,
	SourceMCP:     1,
	SourceGlobal:  2,
	SourceProject: 3,
}

// registryEntry is the registry's per-name slot.
type registryEntry struct {
	cmd     Command
	canon   string // canonical name (matches Definition.Name, lowercased)
	kind    SourceKind
	aliases []string // canonical aliases owned by this entry
}

// Registry is the dispatch table for slash commands. Safe for
// concurrent Lookup, List, and Dispatch; Register serializes
// against itself.
//
// The zero value is unusable — construct via [NewRegistry].
type Registry struct {
	mu       sync.RWMutex
	commands map[string]*registryEntry // canonical name -> entry
	aliases  map[string]string         // alias -> canonical name
}

// NewRegistry returns an empty Registry ready for [Register] calls.
func NewRegistry() *Registry {
	return &Registry{
		commands: make(map[string]*registryEntry),
		aliases:  make(map[string]string),
	}
}

// Register adds a command to the registry. Returns [ErrCollision]
// (wrapped in a [CommandError]) when an existing entry has the
// same canonical name AND the same [SourceKind]. Cross-kind
// collisions follow precedence: a higher-rank registration
// replaces a lower-rank one without error; a lower-rank
// registration is silently ignored.
//
// Validates the Definition: the canonical name must match
// [a-z0-9_-]+, aliases must follow the same rule, no alias may
// collide with another command's canonical name, and no two
// aliases may collide.
//
// Registration is the only public mutation path on the registry.
// Once registered, commands are not removed during a process
// lifetime; restart is the supported way to reset the registry.
func (r *Registry) Register(c Command) error {
	if c == nil {
		return errors.New("command: nil registration")
	}
	def := c.Definition()
	canon := strings.ToLower(def.Name)
	if !validName(canon) {
		return fmt.Errorf("command: invalid name %q", def.Name)
	}
	for _, a := range def.Aliases {
		if !validName(strings.ToLower(a)) {
			return fmt.Errorf("command %q: invalid alias %q", canon, a)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.commands[canon]; ok {
		if existing.kind == def.Source.Kind {
			return &CommandError{Name: canon, Err: ErrCollision}
		}
		newRank := precedenceOrder[def.Source.Kind]
		oldRank := precedenceOrder[existing.kind]
		if newRank <= oldRank {
			// Shadowed by an equal-or-higher precedence registration
			// already present — silently ignore. Returning nil here
			// matches the precedence model (cross-kind collisions are
			// not errors).
			return nil
		}
		// Replace: clear out old aliases owned by the existing entry
		// before installing the new aliases.
		for _, a := range existing.aliases {
			delete(r.aliases, a)
		}
		delete(r.commands, canon)
	}

	canonAliases := make([]string, 0, len(def.Aliases))
	for _, a := range def.Aliases {
		ca := strings.ToLower(a)
		if ca == canon {
			continue // alias matches canonical — silently dedupe
		}
		// An alias must not collide with another command's canonical
		// name or with another alias.
		if _, exists := r.commands[ca]; exists {
			return fmt.Errorf("command %q: alias %q collides with existing command", canon, a)
		}
		if owner, exists := r.aliases[ca]; exists && owner != canon {
			return fmt.Errorf("command %q: alias %q collides with alias on %q", canon, a, owner)
		}
		canonAliases = append(canonAliases, ca)
	}

	entry := &registryEntry{
		cmd:     c,
		canon:   canon,
		kind:    def.Source.Kind,
		aliases: canonAliases,
	}
	r.commands[canon] = entry
	for _, a := range canonAliases {
		r.aliases[a] = canon
	}
	return nil
}

// Lookup resolves a name (or alias) to its registered command.
// Match is case-insensitive on the lookup key; the returned
// command's canonical name is whatever it registered under.
func (r *Registry) Lookup(name string) (Command, bool) {
	key := strings.ToLower(name)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.commands[key]; ok {
		return entry.cmd, true
	}
	if canon, ok := r.aliases[key]; ok {
		if entry, ok := r.commands[canon]; ok {
			return entry.cmd, true
		}
	}
	return nil, false
}

// List returns all registered commands sorted by canonical name.
// Stable output makes /help deterministic.
func (r *Registry) List() []Command {
	r.mu.RLock()
	names := make([]string, 0, len(r.commands))
	for name := range r.commands {
		names = append(names, name)
	}
	r.mu.RUnlock()

	sort.Strings(names)

	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Command, 0, len(names))
	for _, name := range names {
		if entry, ok := r.commands[name]; ok {
			out = append(out, entry.cmd)
		}
	}
	return out
}

// Dispatch parses input as a slash command and runs it through the
// registered handler.
//
// Return contract:
//   - matched=false, err=nil: input is not a slash command. Caller
//     forwards the original input to the agent unchanged.
//   - matched=true, err=nil: command ran successfully. Caller
//     swallows the input.
//   - matched=true, err non-nil: command was matched but failed.
//     err is always a [*CommandError]; use [errors.Is] against the
//     sentinels ([ErrUnknownCommand], [ErrTurnInFlight],
//     [ErrInvalidCommandShape]) or unwrap to inspect the cause.
//     Caller surfaces the error to the user.
//
// Dispatch is safe for concurrent calls. Handlers must themselves
// be safe for concurrent invocation if the frontend can dispatch
// in parallel — Phase 1 frontends serialize input handling, so
// per-call mutual exclusion is the frontend's responsibility, not
// the registry's.
func (r *Registry) Dispatch(
	ctx context.Context, sess Session, input string, opts ...DispatchOption,
) (matched bool, err error) {
	name, args, ok := parseSlash(input)
	if !ok {
		return false, nil
	}

	o := &dispatchOpts{}
	for _, opt := range opts {
		opt(o)
	}

	cmd, found := r.Lookup(name)
	if !found {
		return true, &CommandError{Name: name, Err: ErrUnknownCommand}
	}

	if o.busy != nil && o.busy() {
		return true, &CommandError{Name: name, Err: ErrTurnInFlight}
	}

	switch c := cmd.(type) {
	case HandlerCommand:
		if hErr := c.Handle(ctx, sess, args); hErr != nil {
			return true, &CommandError{Name: name, Err: hErr}
		}
		return true, nil
	case PromptCommand:
		rendered, rErr := c.Render(args)
		if rErr != nil {
			return true, &CommandError{Name: name, Err: rErr}
		}
		if sErr := sess.SubmitPrompt(ctx, rendered); sErr != nil {
			return true, &CommandError{Name: name, Err: sErr}
		}
		return true, nil
	default:
		return true, &CommandError{Name: name, Err: ErrInvalidCommandShape}
	}
}
