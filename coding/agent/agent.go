// Package agent implements the multi-turn LLM loop.
// It communicates with frontends through a typed event channel,
// making it usable from any UI framework (TUI, GUI, web, etc.).
package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/approval"
	"github.com/latebit-io/nib/coding/budget"
	"github.com/latebit-io/nib/coding/nudges"
	"github.com/latebit-io/nib/coding/tools"
	"github.com/latebit-io/nib/engine/event"
	"github.com/latebit-io/nib/engine/lang"
	"github.com/latebit-io/nib/engine/lint"
	"github.com/latebit-io/nib/engine/memory"
	"github.com/latebit-io/nib/engine/runconfig"
	"github.com/latebit-io/nib/engine/validate"
)

// Mode is an alias for event.Mode so existing callers within the agent
// package can use the unqualified names. The canonical definition lives
// in the event package (shared domain types).
type Mode = event.Mode

const (
	// ModeExecution is the default mode: all tools available, execution prompt.
	ModeExecution = event.ModeExecution
	// ModePlanning restricts the agent to read-only and memory tools,
	// using a planning-focused prompt for conversational design.
	ModePlanning = event.ModePlanning
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

// errStaleRun is the sentinel returned by [Agent.processLLMTurn] when
// it detects mid-turn that the agent's runID has advanced (a competing
// RunWithMode/Reply replaced this run). Distinct from context.Canceled
// so [Agent.runLoop] can short-circuit without emitting the post-turn
// AgentWaiting event for a run that has already been replaced — using
// context.Canceled here would route the stale goroutine through the
// normal error path and yield an AgentWaiting for the wrong run.
var errStaleRun = errors.New("agent run replaced by a newer run")

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
// flow. Enforcement happens in [Agent.executeToolCalls].
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
	prompts  *PromptLoader

	mu         sync.Mutex
	cancel     context.CancelFunc
	activeFile string
	intent     string // current developer intent — included in every tool result
	mode       Mode   // current conversation mode (execution or planning)

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
	approvalFlow *approval.Orchestrator

	// waiting is set while the run loop is parked on coord.AwaitInput
	// between turns, awaiting the developer's next message.
	waiting bool
	// running is true while the run() goroutine is alive.
	running bool

	// savedMessages and savedMode preserve the conversation when a run
	// exits (cancel, fatal error). Resume picks these up to continue
	// from where the conversation left off instead of starting fresh.
	savedMessages []llm.Message
	savedMode     Mode

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
	codingStyle *CodingStyleData

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

	// pendingLint holds lint violations from the last edit. When non-empty,
	// processLLMTurn injects a user message before the next LLM call,
	// then clears it. This ensures lint violations are seen as user-priority
	// instructions rather than buried in tool results.
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
	// [StyleEvaluatorPort] so tests can substitute a deterministic stub
	// without spinning up a real LLM provider.
	evaluator StyleEvaluatorPort
	// taskEdits collects edits made during the current task for end-of-task
	// review. Accumulates across many LLM turns; cleared on RunWithMode and
	// on task completion. Named for the task boundary (not turn) — lint and
	// style evaluator fire when the agent marks a task complete, not on
	// every turn.
	taskEdits []taskEdit

	// sessionUsage accumulates token consumption across the entire agent run.
	sessionUsage budget.Session
	// taskTokenBudget caps prompt+completion tokens for a single agent run.
	// Zero means unlimited (the budget check is skipped). Set via
	// [NewOptions.TaskTokenBudget]; the run loop aborts with an AgentError
	// when sessionUsage prompt+completion crosses this threshold.
	taskTokenBudget int
	// budgetExceeded latches once the budget abort fires so subsequent
	// turn checks (e.g. on a Resume of a saved conversation) do not double-
	// emit the AgentError. Reset alongside sessionUsage on RunWithMode/Reply.
	budgetExceeded bool
	// turnCounter is the 1-indexed turn number within the current run.
	turnCounter int
	// runID is a generation token incremented on each RunWithMode call.
	// recordTurnUsage checks this to ignore late updates from canceled runs.
	runID uint64
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
	CodingStyle *CodingStyleData
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
	// developer. Typed as [StyleEvaluatorPort] so callers can inject a
	// stub or alternative implementation; the LLM-backed
	// [*StyleEvaluator] satisfies the interface.
	StyleEvaluator StyleEvaluatorPort
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
	var codingStyle *CodingStyleData
	var linters []lint.Linter
	var evaluator StyleEvaluatorPort
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
		prompts:           NewPromptLoader(projectRoot),
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

	a.approvalFlow = approval.NewOrchestrator(approval.Deps{
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

	return a
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
		tools.NewBashTool(projectRoot),
	}

	if diagProvider != nil {
		builtins = append(builtins, tools.NewDiagnosticsTool(diagProvider, workspace))
	}

	builtins = append(builtins, tools.NewGoToLineTool(workspace, a))
	builtins = append(builtins, tools.NewGlobTool(workspace))
	builtins = append(builtins, tools.NewSearchProjectTool(projectRoot))
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

	// Memory tools — conditionally registered when demarkus is configured.
	if memStore != nil {
		builtins = append(builtins,
			tools.NewMemoryFetchTool(memStore),
			tools.NewMemoryPublishTool(memStore),
			tools.NewMemoryAppendTool(memStore),
			tools.NewMemoryListTool(memStore),
		)
	}

	// Task tracking — conditionally registered via type assertion on workspace.
	// update_task handles activate/complete; project_task_add handles new-task
	// creation; project_init bootstraps /project.md so the work tree is loaded
	// before any of those calls fire on a fresh repo. All three share the same
	// TaskTracker instance so mutations route through the session's in-memory
	// work tree.
	if tt, ok := workspace.(TaskTracker); ok {
		builtins = append(builtins,
			tools.NewTaskTool(tt, a),
			tools.NewProjectTaskAddTool(tt),
			tools.NewProjectInitTool(tt),
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

// Run starts a new conversation in execution mode. See RunWithMode for details.
// Public lifecycle and signal API (Run, RunWithMode, Reply, Cancel,
// IsWaiting, IsRunning, SetProvider/Style/Terse/Autonomous/Evaluator,
// Approve, Reject, Continue, activeCoord, drainPendingLint,
// hasLintPending, currentTerse/Autonomous/CodingStyle/Provider, Usage,
// send, sendCritical) lives in lifecycle.go.

// Per-turn budget integration (recordTurnUsage, shouldAbortForBudget,
// checkTaskBudget, abortIfBudgetExceeded) lives in budget.go.

// Run goroutine + main loop (run, resumeRun, runLoop) live in run.go.

// Post-turn nudges (tryInjectPostTurnNudge, shouldNudgeOutstanding,
// tasksAllComplete) live in nudges.go.

// Per-turn pipeline (planningToolDefs, afterToolDispatch,
// processLLMTurn, executeToolCalls, flushDirtyBuffers, dispatchTool)
// lives in turn.go. Streaming + compaction primitives live in
// [coding/streaming]; truncation recovery in [coding/truncation].

// Truncation handling (Recover, Escalate, RecoveryMessages,
// AppendRejections, MaxRetries) lives in [coding/truncation]. Agent
// invokes [truncation.Recover] from processLLMTurn after a Truncated
// stream event.

// Task-review pipeline (runTaskReview, nextTaskHint, groupEditsByDir,
// runLinters, infraError, runSmokeReview, formatFindings, evaluateTurn)
// lives in task_review.go.

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
// [coding/approval.Orchestrator]. The agent constructs one in New
// with [approval.Deps] callbacks bound to its own state and routes
// proposals through it via [Agent.Propose] in collab_impl.go.

// Agent-internal gates + bookkeeping (recordEdit, maxValidatorRetries,
// fetchMemorySummary, enforceActiveTaskGate, intentReminder) lives
// in gates.go.
