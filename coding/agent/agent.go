// Package agent implements the multi-turn LLM loop.
// It communicates with frontends through a typed event channel,
// making it usable from any UI framework (TUI, GUI, web, etc.).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/editflow"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/nudges"
	"github.com/latebit-io/nib/coding/prompts"
	"github.com/latebit-io/nib/coding/style"
	"github.com/latebit-io/nib/coding/tools"
	"github.com/latebit-io/nib/engine/lang"
	"github.com/latebit-io/nib/engine/lint"
	"github.com/latebit-io/nib/engine/runconfig"
	enginesearch "github.com/latebit-io/nib/engine/search"
	"github.com/latebit-io/nib/engine/validate"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/approval"
	"github.com/latebit-io/nib/kit/budget"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/tools/bash"
	memorytools "github.com/latebit-io/nib/kit/tools/memory"
	searchtools "github.com/latebit-io/nib/kit/tools/search"
)

// InteractionMode controls prompt framing — how the agent describes its
// workflow to the LLM. It does NOT change runtime behavior: the agent
// always awaits approval/continue signals from the [approval.Coordinator]
// for edits, and the frontend (TUI or headless Runner) is responsible
// for driving those signals.
// Set at construction time, immutable for the agent's lifetime.
type InteractionMode int

const (
	// Interactive is the default: prompts describe an editor with a developer
	// who reviews and approves edits one at a time.
	Interactive InteractionMode = iota
	// Headless: prompts describe autonomous operation where edits are applied
	// directly. The headless Runner auto-signals approval channels, so the
	// agent runs at full speed without user interaction.
	Headless
)

// coordCtxKey is the unexported context-value key under which the
// active run's [*approval.Coordinator] is stashed. Used to bridge
// the tool boundary: the [tools.Approver.Propose] collaborator
// method is invoked by tools (which have no coord in scope) and
// needs to dispatch to the run's coordinator. Every other agent-
// internal callsite receives coord as an explicit parameter.
type coordCtxKey struct{}

// ctxWithCoord returns ctx annotated with c. Called once per run, on
// the goroutine-launching side, so the run goroutine and any tool
// dispatched on its behalf retrieve the run's coordinator via
// [coordFromCtx] without reading the lazily-mutated [Agent.coord]
// field.
func ctxWithCoord(ctx context.Context, c *approval.Coordinator) context.Context {
	return context.WithValue(ctx, coordCtxKey{}, c)
}

// coordFromCtx extracts the coordinator stashed by [ctxWithCoord].
// Returns nil when the ctx was not annotated — a programming error
// that callers should surface, not silently swallow.
func coordFromCtx(ctx context.Context) *approval.Coordinator {
	c, _ := ctx.Value(coordCtxKey{}).(*approval.Coordinator)
	return c
}

// mutatingTools contains tool names that modify filesystem or shell state.
// In execution mode these require an active `[>]` task in /project.md —
// the gate enforces "all agent work is tracked in the project tree."
var mutatingTools = map[string]bool{
	"edit_file":    true,
	"write_file":   true,
	"replace_file": true,
	"bash":         true,
	"smoke_run":    true,
}

// fileEditTools is the subset of mutating tools whose calls must be
// rate-limited to ONE per LLM turn in non-autonomous, non-headless mode.
// Distinct from [mutatingTools] (which gates on the project task tree)
// because bash and smoke_run can legitimately chain after an edit (e.g.
// "edit then verify with go test"), but a second file edit in the same
// turn means the model is bypassing the developer's review-and-continue
// flow. Enforced by [Agent.singleEditGate] in BeforeToolCall.
var fileEditTools = map[string]bool{
	"edit_file":    true,
	"write_file":   true,
	"replace_file": true,
}

// Agent drives the multi-turn LLM loop.
type Agent struct {
	provider llm.Provider
	events   chan<- event.Event // frontend reads from this
	tools    map[string]Tool
	toolDefs []llm.ToolDef
	cache    *FileCache
	prompts  *prompts.PromptLoader

	mu         sync.Mutex
	cancel     context.CancelFunc
	activeFile string
	intent     string     // current developer intent — included in every tool result
	mode       event.Mode // current conversation mode (execution or planning)

	// coord owns the four coordination channels (approve/continue/
	// reply/answer) the agent uses to talk to the frontend. Replaced
	// (not reset) at every run boundary so a stale goroutine parked
	// in coord.Await* on the previous run cannot consume signals
	// meant for the new run.
	coord *approval.Coordinator

	// approvalFlow runs the edit-approval orchestration (validation,
	// proposal delivery, await approve/reject, recordEdit, await
	// continue, diagnostics). Constructed once at New() time with
	// callbacks that close over agent state; the per-run coord is
	// passed to Handle from the run goroutine via coordFromCtx.
	approvalFlow *editflow.Orchestrator

	// waiting is set while the run loop is parked on coord.AwaitInput
	// between turns, awaiting the developer's next message.
	waiting bool
	// running is true while the run() goroutine is alive.
	running bool

	// diagProvider is optionally set to auto-inject diagnostics after edits.
	diagProvider lang.DiagnosticProvider
	// diagDelay is the wait time for the language server to push diagnostics after an edit.
	diagDelay time.Duration

	// workspace is used by the approval flow to manage context set.
	workspace Workspace

	// planningBlocklist is the per-instance set of tool names blocked in planning mode.
	planningBlocklist map[string]bool

	// interactionMode controls prompt framing only (interactive vs headless).
	// Does not affect runtime behavior — approval channels are always used.
	interactionMode InteractionMode

	// memoryStore is used to re-fetch the session summary before each goal.
	memoryStore memory.Store
	// memorySummary is the fallback summary from startup, used when re-fetch fails.
	memorySummary string

	// distributedMemory lists MCP server names recognized as shared/team memory.
	// Injected into the system prompt so the agent distinguishes local from shared.
	distributedMemory []string

	// codingStyle holds the active coding style rules for prompt injection.
	// Nil when no style is configured.
	codingStyle *prompts.CodingStyleData

	// linters is the set of lint adapters to run at task completion.
	// Nil when no linter is configured. Each adapter returns a structured
	// lint.Result distinguishing infrastructure failures from findings.
	linters []lint.Linter

	// pipeline runs pre-approval validators (syntactic parse, LSP shadow,
	// style invariants) against a proposed edit before the comprehension
	// gate sees it. Initialised to validate.NoopPipeline in New so call
	// sites dispatch unconditionally; swapped for a live pipeline at the
	// composition root once adapters are registered.
	pipeline validate.Pipeline

	// validatorRetries counts per-path silent retries triggered by
	// validate.Retry verdicts. The LLM gets MaxValidatorRetries chances
	// to self-correct a broken proposal before the bad version is
	// surfaced to the developer. Reset on successful approval and on
	// run boundaries.
	validatorRetries map[string]int

	// pendingLint holds lint violations from the last edit. When
	// non-empty, [Agent.foundationCompactAndLint] drains it as a user
	// message into the next Stream call. This ensures lint violations
	// are seen as user-priority instructions rather than buried in tool
	// results.
	pendingLint string

	// smokeConfig is the resolved smoke-run configuration. Skipped means
	// no smoke command is available for this project; the agent does not
	// register the smoke_run tool in that case and runTaskReview skips
	// the auto-invocation. Read-only after New.
	smokeConfig runconfig.Resolved

	// terse enables terse output mode — instructs the LLM to minimize
	// explanatory text, reducing output tokens by ~65%.
	terse bool

	// autonomous relaxes one-edit-at-a-time constraints so the agent
	// works continuously without stopping between edits.
	autonomous bool

	// evaluator is the optional style evaluator that reviews edits after
	// each turn completes. Nil when the feature is disabled. Typed as
	// [style.StyleEvaluatorPort] so tests can substitute a deterministic stub
	// without spinning up a real LLM provider.
	evaluator style.StyleEvaluatorPort
	// taskEdits collects edits made during the current task for end-of-task
	// review. Accumulates across many LLM turns; cleared on RunWithMode and
	// on task completion. Named for the task boundary (not turn) — lint and
	// style evaluator fire when the agent marks a task complete, not on
	// every turn.
	taskEdits []taskEdit

	// Per-run session usage lives on [providerProxy.session] —
	// accumulated synchronously inside the Stream channel wrapper so
	// the foundation's pre-Stream budget check ([Agent.foundationBudgetCheck])
	// sees fresh totals on the immediately following turn. Read via
	// [providerProxy.Snapshot]; reset via [providerProxy.ResetSession]
	// on each new run.

	// taskTokenBudget caps prompt+completion tokens for a single agent run.
	// Zero means unlimited (the budget check is skipped). Set via
	// [NewOptions.TaskTokenBudget]; the run loop aborts with an AgentError
	// when the session prompt+completion crosses this threshold.
	taskTokenBudget int
	// budgetExceeded latches once the budget abort fires so subsequent
	// turn checks (e.g. on a Resume of a saved conversation) do not double-
	// emit the AgentError. Reset on RunWithMode/Reply alongside the
	// providerProxy's session counter.
	budgetExceeded bool
	// runUnsuccessful is set by [Agent.send] when an [event.AgentError]
	// is emitted and by [Agent.Cancel]. [Agent.forwardKitEvents] reads
	// it on [event.AgentDone] to override the kit-derived Success flag
	// — kit's per-run outcome only knows about Aborts and foundation
	// Errors, so coding-side AgentErrors emitted via [Agent.send]
	// (autosave failure, post-turn budget, RunWithMode rejection) need
	// this wrapper-level flag to surface as Success=false. Reset on
	// each new run.
	runUnsuccessful bool
	// turnCounter is the 1-indexed turn number within the current run.
	turnCounter int
	// Per-turn estimate emission and AgentTurnUsage pairing live on
	// [providerProxy] — its Stream wrapper is the only point where
	// the estimate (computed pre-Stream from the exact msgs+tools the
	// provider sees) can be committed alongside the post-Stream
	// usage WITHOUT depending on kit's lossy AgentTurnUsage delivery.
	// A previous design used a forwarder-side FIFO queue, but kit
	// drops AgentTurnUsage on full consumer channels and the queue
	// would desync indefinitely after the first drop.
	// truncationRetries counts truncated turns within the current run.
	// Read + incremented by the OnTruncated foundation hook; reset to
	// zero on each new run alongside the other per-run state. Lives on
	// the agent (rather than as closure state in [Agent.FoundationHooks])
	// because FoundationHooks is built once at [New] time — closure
	// state would persist across runs and cause a previous run's
	// truncations to count against the next run's [truncationMaxRetries]
	// budget.
	truncationRetries int

	// providerProxy is the [llm.Provider] handed to [kit.New]. It
	// shadows [Agent.provider] so [SetProvider] can hot-swap the live
	// provider mid-session even though the kit/foundation captures its
	// provider once at construction time.
	providerProxy *providerProxy

	// kit is the embeddable [kit.Agent] that drives the run loop.
	// Built in [New] with [providerProxy], [kitEvents], and the
	// hooks composed by [Agent.FoundationHooks].
	kit *kit.Agent

	// kitEvents is the channel [kit] emits its [event.Event] stream on.
	// Drained by [Agent.forwardKitEvents] — the application boundary
	// where the generic agent events get augmented with coding-specific
	// per-turn budget accounting and forwarded to the frontend channel.
	kitEvents chan event.Event

	// forwardDone is closed by [Agent.forwardKitEvents] (via defer)
	// when the forwarder goroutine returns. [Agent.Close] blocks on
	// it after closing [Agent.kitEvents] so the wrapper provides the
	// same "no further writes to the consumer channel" guarantee
	// kit.Agent.Close provides for [Config.Events] — without this
	// wait, a consumer that closes the frontend events channel right
	// after Close returns can race a forwarder still draining the
	// last buffered AgentDone.
	forwardDone chan struct{}

	// closeOnce guards [Agent.Close] so concurrent or repeated Close
	// calls do not double-close the kit agent or its event channel.
	closeOnce sync.Once

	// startMu serializes the entire run-startup sequence
	// (cancel-previous + kit.WaitForIdle + fenceForwarder + per-run
	// state reset + armRunDone + kit.PromptWithMessages). Held by
	// RunWithMode and by Reply's resume path; runtime signals
	// (Cancel, Approve, Reject, IsRunning, IsWaiting) acquire only
	// [Agent.mu] so they cannot deadlock against an in-flight
	// startup. Without this lock, two concurrent starters that both
	// passed fenceForwarder could interleave their [Agent.mu]-serialized
	// resets and arm calls, leaving the foundation run that wins
	// kit.PromptWithMessages executing against the loser's wrapper
	// state ([Agent.coord], [Agent.cancel], [Agent.intent], [Agent.mode]),
	// which would route Approve/Reject signals to the wrong run.
	startMu sync.Mutex

	// runDoneMu guards [Agent.runDone]. Forwarder writes (closes the
	// channel + nil-clears the field on AgentDone). Run-start methods
	// read it (block on <- before resetting per-run state) and
	// allocate a fresh channel for the new run.
	runDoneMu sync.Mutex
	// runDone is closed by [Agent.forwardKitEvents] when the active
	// run's [event.AgentDone] has been fully processed. Run-start
	// methods (RunWithMode, Reply's resume path) [Agent.fenceForwarder]
	// on the previously-installed channel before resetting per-run
	// state ([Agent.sessionUsage], [Agent.turnCounter],
	// [Agent.runUnsuccessful], [Agent.budgetExceeded], [Agent.running])
	// and starting the next run — without the fence, a forwarder still
	// processing the prior run's buffered events would mutate the new
	// run's state and (worst case) charge the prior run's tokens
	// against the new run's budget.
	runDone chan struct{}
}

// taskEdit records a single edit made during an agent task, for end-of-task
// batch review (lint, style evaluator).
type taskEdit struct {
	Path    string
	Search  string
	Replace string
}

// NewOptions holds optional dependencies for agent construction.
type NewOptions struct {
	// DiagProvider enables diagnostics tool and auto-injection after edits.
	// Nil when no language service is available.
	DiagProvider lang.DiagnosticProvider
	// MemoryStore enables memory tools (fetch, publish, append, list).
	// Nil when demarkus is not configured.
	MemoryStore memory.Store
	// MemorySummary is the project memory snapshot injected into the first prompt.
	// Empty string when memory is not configured or no summary exists yet.
	MemorySummary string
	// PlanningBlocklist adds extra tool names to block during planning mode.
	// These are merged with the built-in defaults (edit_file, write_file, bash);
	// they extend the blocklist, not replace it.
	PlanningBlocklist []string
	// Interaction sets the prompt framing (Interactive vs Headless).
	// This only affects system prompt text — it does not change runtime
	// behavior. The frontend must handle approval signaling regardless.
	Interaction InteractionMode
	// DistributedMemory lists MCP server names recognized as shared/team
	// memory (detected by naming convention). When non-empty, the system
	// prompt includes a section explaining how to use local vs shared memory.
	DistributedMemory []string
	// CodingStyle holds the resolved coding style. When non-nil, style rules
	// are injected into the system prompt as architectural constraints.
	CodingStyle *prompts.CodingStyleData
	// Linters is the set of lint adapters to run at task completion. Each
	// adapter returns a structured lint.Result (findings or error). Nil
	// disables post-task lint. Use lint.Detect or lint.FromShellCommands to
	// populate. The previous StyleLintCmd []string has been replaced — a
	// shell-command adapter is available via lint.FromShellCommands.
	Linters []lint.Linter
	// ValidationPipeline runs pre-approval validators against proposed
	// edits (syntactic parse, LSP shadow, style invariants). Nil installs
	// validate.NoopPipeline so call sites dispatch without nil checks.
	ValidationPipeline validate.Pipeline
	// StyleEvaluator is the optional style reviewer. When non-nil, proposed
	// edits are reviewed against style rules before being shown to the
	// developer. Typed as [style.StyleEvaluatorPort] so callers can inject a
	// stub or alternative implementation; the LLM-backed
	// [*style.StyleEvaluator] satisfies the interface.
	StyleEvaluator style.StyleEvaluatorPort
	// Terse enables terse output mode at startup. When true, the system
	// prompt instructs the LLM to minimize explanatory text, reducing
	// output tokens by ~65%. Switchable at runtime via SetTerse.
	Terse bool
	// SmokeConfig is the resolved smoke-run configuration for the
	// project. When non-Skipped, the agent registers the smoke_run
	// tool and auto-invokes it during runTaskReview so tasks marked
	// complete are verified by an actual launch — runtime errors that
	// the parser/lint/architecture stages cannot catch surface here.
	SmokeConfig runconfig.Resolved

	// TaskTokenBudget caps total prompt+completion tokens consumed by a
	// single agent run (RunWithMode → Done). Once the budget is exceeded
	// the run aborts with an AgentError so a runaway loop cannot quietly
	// burn the developer's wallet. The 2026-04-26 pacman regression burned
	// 55M+ tokens on a single broken task; the budget is the safety net
	// that keeps that from recurring.
	//
	//	== 0 — use the default ([defaultTaskTokenBudget]).
	//	 < 0 — unlimited (disable the check; not recommended).
	//	 > 0 — explicit cap in tokens.
	TaskTokenBudget int
}

// New creates an agent with the given provider, workspace, and tools.
// The project root is derived from workspace.ProjectRoot().
// The opts parameter is optional — pass nil for defaults.
// The frontend must continuously drain the events channel. Streaming events
// (AgentToken, AgentStatus) are dropped when the channel is full; control-flow
// events (EditProposed, Done, Error) block for up to 5 seconds before being
// discarded with a log. Use a buffered channel (e.g. 64) to absorb bursts.
func New(provider llm.Provider, workspace Workspace, events chan<- event.Event, opts *NewOptions, extraTools ...Tool) *Agent {
	coord := approval.New()
	cache := NewFileCache()
	projectRoot := workspace.ProjectRoot()

	var diagProvider lang.DiagnosticProvider
	var memStore memory.Store
	var memorySummary string
	var extraBlocklist []string
	var interaction InteractionMode
	var distributedMemory []string
	var codingStyle *prompts.CodingStyleData
	var linters []lint.Linter
	var evaluator style.StyleEvaluatorPort
	var terse bool
	var smokeCfg runconfig.Resolved
	var pipeline validate.Pipeline = validate.NoopPipeline{}
	var taskTokenBudgetInput int
	if opts != nil {
		diagProvider = opts.DiagProvider
		memStore = opts.MemoryStore
		memorySummary = opts.MemorySummary
		extraBlocklist = opts.PlanningBlocklist
		interaction = opts.Interaction
		distributedMemory = opts.DistributedMemory
		codingStyle = opts.CodingStyle
		linters = slices.Clone(opts.Linters)
		evaluator = opts.StyleEvaluator
		terse = opts.Terse
		smokeCfg = opts.SmokeConfig
		if opts.ValidationPipeline != nil {
			pipeline = opts.ValidationPipeline
		}
		taskTokenBudgetInput = opts.TaskTokenBudget
	}
	taskTokenBudget := budget.Resolve(taskTokenBudgetInput)

	// Build per-instance planning blocklist: start from defaults, merge extras.
	// DefaultPlanningBlocklist returns a fresh copy so mutations stay local.
	merged := nudges.DefaultPlanningBlocklist()
	for _, name := range extraBlocklist {
		merged[strings.ToLower(name)] = true
	}

	a := &Agent{
		provider:          provider,
		events:            events,
		cache:             cache,
		prompts:           prompts.NewPromptLoader(projectRoot),
		coord:             coord,
		diagProvider:      diagProvider,
		planningBlocklist: merged,
		interactionMode:   interaction,
		memoryStore:       memStore,
		memorySummary:     memorySummary,
		distributedMemory: distributedMemory,
		codingStyle:       codingStyle,
		linters:           linters,
		pipeline:          pipeline,
		validatorRetries:  make(map[string]int),
		terse:             terse,
		evaluator:         evaluator,
		diagDelay:         500 * time.Millisecond,
		workspace:         workspace,
		smokeConfig:       smokeCfg,
		taskTokenBudget:   taskTokenBudget,
	}

	a.approvalFlow = editflow.NewOrchestrator(editflow.Deps{
		Cache:        cache,
		Workspace:    workspace,
		Send:         a.send,
		SendCritical: a.sendCritical,
		Validate:     a.runValidationPipeline,
		RecordEdit:   a.recordEdit,
		DiagProvider: diagProvider,
		DiagDelay:    a.diagDelay,
	})

	a.registerTools(workspace, cache, projectRoot, diagProvider, memStore, extraTools)

	a.buildKitAgent()

	return a
}

// buildKitAgent constructs [Agent.kit], the [kit.Agent] that drives the
// run loop. Wires the [providerProxy] (so SetProvider survives the
// foundation's frozen provider field), an intercept events channel
// drained by [Agent.forwardKitEvents], and the hooks composed by
// [Agent.FoundationHooks].
//
// The Tool slice is iterated in [Agent.toolDefs] order so the kit/
// foundation's internal toolDefs match the advertised order — keeping
// LLM tool-list ordering stable so model behavior is reproducible
// across runs.
//
// Validation failures from [kit.New] panic: inputs are statically known
// at this composition root (proxy never nil, events channel allocated,
// tool definitions vetted by [registerTools] which drops duplicates),
// so a non-nil error indicates a programming error caught at startup
// rather than a runtime condition.
func (a *Agent) buildKitAgent() {
	// Fail fast on nil provider. kit.New only sees the providerProxy
	// (always non-nil) so its own provider-validation cannot catch a
	// nil [Agent.provider] — without this guard the nil interface
	// would propagate through the proxy and panic on the first
	// Stream call instead of at construction.
	if a.provider == nil {
		panic("agent.New: provider is required")
	}
	a.providerProxy = newProviderProxy(a.provider, a.send, a.onTurnSettled)

	kitTools := make([]kit.Tool, 0, len(a.toolDefs))
	for _, def := range a.toolDefs {
		t, ok := a.tools[strings.ToLower(def.Function.Name)]
		if !ok {
			panic(fmt.Sprintf("agent.New: tool %q advertised in toolDefs but missing from tools map", def.Function.Name))
		}
		kitTools = append(kitTools, t)
	}

	a.kitEvents = make(chan event.Event, 64)
	a.forwardDone = make(chan struct{})

	hooks := a.FoundationHooks(func() []llm.Message {
		k := a.kit
		if k == nil {
			return nil
		}
		return k.State().Messages
	})

	kitAgent, err := kit.New(kit.Config{
		Provider: a.providerProxy,
		Events:   a.kitEvents,
		Tools:    kitTools,
		Hooks:    hooks,
	})
	if err != nil {
		panic(fmt.Sprintf("agent.New: kit construction failed: %v", err))
	}
	a.kit = kitAgent

	go a.forwardKitEvents()
}

// Close gracefully shuts down the agent. Aborts any in-flight run,
// waits for the kit/foundation AND the kit translator to unwind,
// closes the intercept events channel so [Agent.forwardKitEvents]
// drains and exits, then waits for the forwarder. Idempotent.
//
// Ordering matters and is the contract callers rely on:
//
//  1. kit.Close blocks until the kit translator has fully exited
//     (no more writes to [Agent.kitEvents]) — otherwise step 3 would
//     race a still-running translator and panic on send-on-closed.
//  2. close(kitEvents) signals the forwarder to drain its remaining
//     events and return.
//  3. <-forwardDone blocks until forwarder exits, providing the
//     symmetric "no further writes to the frontend events channel"
//     guarantee. A consumer that closes the frontend channel
//     immediately after Close returns is safe.
//
// After Close, calls into [Agent.RunWithMode], [Agent.Run], and
// [Agent.Reply] will fail to start a new run (kit returns ErrClosed).
// The frontend events channel passed to [New] is NOT closed — that
// belongs to the caller.
//
// Close hangs if the consumer of the frontend events channel has
// stopped draining — same hard contract as steady-state operation.
func (a *Agent) Close() {
	a.closeOnce.Do(func() {
		if a.kit != nil {
			a.kit.Close()
		}
		if a.kitEvents != nil {
			close(a.kitEvents)
		}
		if a.forwardDone != nil {
			<-a.forwardDone
		}
	})
}

// registerTools builds the tool registry. Built-in tools are registered first
// and cannot be overridden by extraTools (e.g. MCP).
//
// Side-effecting tools (edit_file, replace_file, write_file, go_to_line,
// update_task) receive narrow collaborator interfaces that the agent
// satisfies via methods on *Agent (Propose, FileCreated, Navigate,
// OnComplete). The collaborators stay private to this package; the
// tools depend on the abstract interfaces only.
func (a *Agent) registerTools(workspace Workspace, cache *FileCache, projectRoot string, diagProvider lang.DiagnosticProvider, memStore memory.Store, extraTools []Tool) {
	editTool := tools.NewEditFileTool(workspace, cache, a)

	builtins := []Tool{
		tools.NewReadFileTool(workspace, cache),
		editTool,
		tools.NewWriteFileTool(workspace, cache, a),
		tools.NewReplaceFileTool(workspace, cache, a),
		tools.NewListFilesTool(workspace),
		bash.New(projectRoot),
	}

	if diagProvider != nil {
		builtins = append(builtins, tools.NewDiagnosticsTool(diagProvider, workspace))
	}

	builtins = append(builtins, tools.NewGoToLineTool(workspace, a))
	builtins = append(builtins, tools.NewGlobTool(workspace))
	builtins = append(builtins, searchtools.New(projectRoot, adaptEngineSearch))
	builtins = append(builtins, tools.NewPackageInfoTool(projectRoot))
	builtins = a.appendSmokeTool(builtins, projectRoot)

	// request_input was deliberately removed: the LLM was using it as a
	// workaround for tool-surface friction ("which strategy should I
	// pick?") rather than for genuine ambiguity in the developer's
	// goal. With replace_file now covering the wholesale-rewrite case
	// the friction is gone — and the absence of request_input forces
	// the LLM to either make a tool call that succeeds or fail loud,
	// which is a better default than escalating decisions back through
	// a chat prompt for the developer to dis/approve. Re-register
	// behind a feature flag if a legitimate use case appears.

	// LSP-powered tools — conditionally registered via type assertion.
	if diagProvider != nil {
		if dp, ok := diagProvider.(lang.DefinitionProvider); ok {
			builtins = append(builtins, tools.NewGoToDefinitionTool(workspace, dp))
		}
		if rp, ok := diagProvider.(lang.ReferenceProvider); ok {
			builtins = append(builtins, tools.NewFindReferencesTool(workspace, rp))
		}
		if sp, ok := diagProvider.(lang.SymbolProvider); ok {
			builtins = append(builtins, tools.NewWorkspaceSymbolsTool(workspace, sp))
		}
	}

	// Task tracking — conditionally registered via type assertion on workspace.
	// update_task handles activate/complete; project_task_add handles new-task
	// creation; project_init bootstraps /project.md so the work tree is loaded
	// before any of those calls fire on a fresh repo. All three share the same
	// TaskTracker instance so mutations route through the session's in-memory
	// work tree.
	tt, hasTaskTracker := workspace.(TaskTracker)
	if hasTaskTracker {
		builtins = append(builtins,
			tools.NewTaskTool(tt, a),
			tools.NewProjectTaskAddTool(tt),
			tools.NewProjectInitTool(tt),
		)
	}

	// Memory tools — conditionally registered when demarkus is configured.
	// The publish/append validators enforce coding's /project.md schema
	// (see [memory_validators.go]); kit-side tools stay generic.
	// The append validator steers the LLM toward task tools, so it is only
	// wired when the TaskTracker tools are actually registered.
	if memStore != nil {
		var appendOpts []memorytools.Option
		if hasTaskTracker {
			appendOpts = append(appendOpts, memorytools.WithValidator(appendProjectMDValidator))
		}
		builtins = append(builtins,
			memorytools.NewFetchTool(memStore),
			memorytools.NewPublishTool(memStore, memorytools.WithValidator(publishProjectMDValidator)),
			memorytools.NewAppendTool(memStore, appendOpts...),
			memorytools.NewListTool(memStore),
		)
	}

	a.tools = make(map[string]Tool, len(builtins)+len(extraTools))
	a.toolDefs = make([]llm.ToolDef, 0, len(builtins)+len(extraTools))

	builtinNames := make(map[string]bool, len(builtins))
	for _, t := range builtins {
		def := t.Definition()
		key := strings.ToLower(def.Function.Name)
		builtinNames[key] = true
		a.tools[key] = t
		a.toolDefs = append(a.toolDefs, def)
	}

	for _, t := range extraTools {
		def := t.Definition()
		key := strings.ToLower(def.Function.Name)
		if builtinNames[key] {
			slog.Warn("extra tool rejected: shadows built-in", "name", def.Function.Name)
			continue
		}
		if _, exists := a.tools[key]; exists {
			slog.Warn("extra tool collision, skipping duplicate", "name", def.Function.Name)
			continue
		}
		a.tools[key] = t
		a.toolDefs = append(a.toolDefs, def)
	}
}

// adaptEngineSearch bridges engine/search.Search to kit's SearchFunc
// signature so the kit-level search tool stays free of engine imports.
func adaptEngineSearch(_ context.Context, root, pattern string, opts searchtools.Options) ([]searchtools.Result, error) {
	results, err := enginesearch.Search(root, pattern, enginesearch.Options{
		CaseSensitive: opts.CaseSensitive,
		Regex:         opts.Regex,
		MaxResults:    opts.MaxResults,
		FileGlob:      opts.FileGlob,
	})
	if err != nil {
		return nil, err
	}
	out := make([]searchtools.Result, len(results))
	for i, r := range results {
		out[i] = searchtools.Result{Path: r.Path, Line: r.Line, Text: r.Text}
	}
	return out, nil
}

// Public lifecycle and signal API (Run, RunWithMode, Reply, Cancel,
// IsWaiting, IsRunning, SetProvider / Style / Terse / Autonomous /
// Evaluator, Approve, Reject, activeCoord, drainPendingLint,
// hasLintPending, currentTerse / Autonomous / CodingStyle / Provider /
// Mode, Usage, emitOpening, send, sendCritical) lives in lifecycle.go.

// Per-run budget integration (checkTaskBudget) lives in budget.go.
// Per-turn accumulation lives in [Agent.augmentAndAccumulate]
// (forwarder.go). The pure budget math + types live in [kit/budget].

// Kit event forwarding (forwardKitEvents) lives in forwarder.go. The
// forwarder drains the [kit.Agent]'s event stream, augments
// AgentTurnUsage with client-side estimates + sessionUsage
// accounting, overrides AgentDone's Success flag from
// [Agent.runUnsuccessful], and forwards every event to the frontend
// channel.

// Foundation hook bridge (FoundationHooks + every gate it composes)
// lives in foundation_hooks.go. The kit/foundation captures these
// once at construction; closure-scoped per-turn state stays out of
// the [Agent] struct.

// Wrapper-side I/O helpers (planningToolDefs, flushDirtyBuffers) live
// in io.go — wrapper-private utilities the foundation hooks call
// into.

// Post-turn nudge math (shouldNudgeOutstanding, tasksAllComplete)
// lives in nudges.go. The string-pattern detectors themselves live in
// [coding/nudges]; nudges.go is the agent-side glue.

// Streaming + compaction primitives live in [coding/streaming];
// truncation recovery in [coding/truncation].

// Task-review pipeline (runTaskReview, nextTaskHint, groupEditsByDir,
// runLinters, infraError, runSmokeReview, formatFindings,
// evaluateTurn) lives in task_review.go.

// appendSmokeTool registers the smoke_run tool when the project's
// smokeConfig is non-Skipped and resolves to a real command. Extracted
// from registerTools so the latter stays under the func-length cap and
// the gate logic is easier to review in isolation.
//
// Honours [brand.EnvKeySmokeDisabled] end-to-end: when set, the
// tool is NOT advertised to the LLM at all, matching the
// auto-invocation suppression in runTaskReview. Without this guard
// the kill-switch was a half-disable — auto-runs went silent but the
// LLM could still invoke smoke_run directly, which is the opposite
// of what an operator setting the env var wants. Read at agent-
// construction time, matching how [brand.EnvKeyValidatorsDisabled]
// gates the validation pipeline at the composition root; flipping
// the env var mid-session does not toggle live behaviour.
func (a *Agent) appendSmokeTool(builtins []Tool, projectRoot string) []Tool {
	if os.Getenv(brand.EnvKeySmokeDisabled) != "" {
		return builtins
	}
	if a.smokeConfig.Skipped {
		return builtins
	}
	if a.smokeConfig.Command == "" {
		return builtins
	}
	return append(builtins, tools.NewSmokeRunTool(projectRoot, a.smokeConfig))
}

// Edit-approval orchestration (handleEditProposal, waitForApproval,
// waitForContinue, fatalProposalMarkers) lives in
// [coding/editflow.Orchestrator]. The agent constructs one in New
// with [editflow.Deps] callbacks bound to its own state and routes
// proposals through it via [Agent.Propose] in collab_impl.go.

// Agent-internal gates + bookkeeping (recordEdit, maxValidatorRetries,
// fetchMemorySummary, enforceActiveTaskGate, intentReminder) lives
// in gates.go.
