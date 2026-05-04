# nib

> **Craftsmanship at agent speed.**

A coding agent that develops projects end-to-end — for developers who want agentic speed without losing craftsman control. The agent writes the code, respects the architecture, follows the language's idioms, plans against a work tree, and remembers across sessions. **The agent is the product** — embeddable, extensible, frontend-agnostic. Everything else is built on top of it.

This repo also ships an opinionated reference **TUI** — a polished add-on that turns the agent into a daily-driver experience. Three panes — project, editor, agent — give you live observability and high-leverage intervention. Edits stream into a real editor with real syntax highlighting, not a wall of streaming chat. An autonomy dial controls how often you're asked: **Trusted** (default; agent runs, routine edits land), **Guided** (drop here for more control; every edit waits for approval), or **Yolo** (just do it). A headless mode ships today for CI and scripting; the long-term direction is a stable surface and a custom-tool registry so others can build their own frontends, plug in their own tools, or use the agent as the foundation of a different product.

This isn't pair programming. The agent doesn't co-type alongside you, and you don't take turns at the keyboard. The editor (in the reference TUI) is your read view and intervention surface — you watch edits stream in, approve them, and steer the run when it drifts off-course.

## Reference TUI Architecture: Composite TUI Pattern

The reference TUI uses a **Composite TUI** architecture inspired by WPF/Prism, adapted for Go and [Bubble Tea](https://github.com/charmbracelet/bubbletea). The pattern solves a common problem in terminal UIs: as you add panes, panels, and features, the main model becomes a god object that routes every event, manages every layout detail, and tangles every concern together.

This pattern is general-purpose. It works for any Bubble Tea application with multiple independent UI regions.

### The Problem

A typical Bubble Tea app starts simple:

```go
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    // 50 lines of key handling
    // 30 lines of mouse handling
    // 20 lines of layout
    // domain logic mixed in everywhere
}
```

Add a second pane and it doubles. Add a third and it's unmaintainable. Every new feature touches the same function. Key handling, mouse routing, layout, and domain logic are all tangled.

### The Solution: Four Layers

```
┌─────────────────────────────────────────┐
│            Panes (self-contained)       │
│  Each implements Update/Render/SetSize  │
├─────────────────────────────────────────┤
│            RegionManager               │
│  Layout, focus, composition, mouse     │
├─────────────────────────────────────────┤
│            AppModel (orchestrator)      │
│  Cross-pane actions only               │
├─────────────────────────────────────────┤
│            Services (shared)           │
│  Clipboard, LSP, filesystem, etc.      │
└─────────────────────────────────────────┘
```

---

### Layer 1: Pane Interface

Each UI region is a **self-contained module** that owns its own key handling, mouse handling, scroll state, and rendering.

```go
type Pane interface {
    Update(msg tea.Msg) tea.Cmd    // handles keys, mouse, domain messages
    Render() string                // renders the pane to a string
    SetSize(width, height int)     // resize callback (clamp scroll, reflow, etc.)
}
```

A pane knows nothing about other panes. It receives events in its local coordinate space and renders within its allocated dimensions. If it needs to communicate outward, it emits a typed `tea.Msg` via a `tea.Cmd`:

```go
// Agent pane emits this when user submits a goal.
// It doesn't know what happens next — that's the orchestrator's job.
type GoalSubmittedMsg struct {
    Goal string
}

func (m *AgentPane) handleEnter() tea.Cmd {
    goal := m.inputBuffer
    m.inputBuffer = ""
    return func() tea.Msg { return GoalSubmittedMsg{Goal: goal} }
}
```

**Why this works:** Panes are testable in isolation. You can send them messages and assert on their state without standing up the entire application. Adding a new pane doesn't require modifying existing panes.

---

### Layer 2: RegionManager

The RegionManager owns all spatial concerns. The orchestrator delegates to it entirely — no layout code in AppModel.

```go
rm := NewRegionManager(Horizontal)
rm.Add("editor", editorPane, 0.7)    // 70% of width
rm.Add("sidebar", sidebarPane, 0.3)  // 30% of width
```

**What it does:**
- **Layout calculation** — distributes space by ratio, enforces minimum widths, auto-hides panes that don't fit
- **Focus management** — `FocusNext()`, `FocusByName()`, tracks which pane receives key events
- **View composition** — renders all visible panes and joins them with dividers
- **Mouse hit testing** — `HandleMouse(msg)` determines which pane was clicked, translates global coordinates to pane-local coordinates, and forwards the event

```go
// In AppModel.Update — mouse handling is one line:
case tea.MouseMsg:
    cmd := m.Regions.HandleMouse(msg)
    return m, cmd
```

**Why this matters:** When you add a third pane, you call `rm.Add()`. Layout, focus cycling, view composition, and mouse routing all work automatically. No new code in the orchestrator.

---

### Layer 3: AppModel (Thin Orchestrator)

AppModel handles **only cross-cutting concerns** — actions that touch multiple panes or wire things together. Everything else is delegated.

```go
func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    // Dialog captures all input when active
    if m.Dialog.Active {
        return m, m.Dialog.Update(msg)
    }

    switch msg := msg.(type) {
    case tea.WindowSizeMsg:
        m.Regions.SetSize(msg.Width, msg.Height)
        return m, nil

    case tea.MouseMsg:
        return m, m.Regions.HandleMouse(msg)

    // Cross-pane message: agent pane submitted a goal → wire to agent + editor
    case GoalSubmittedMsg:
        m.agent.Run(m.editor.BufferContent(), msg.Goal)
        return m, nil

    case tea.KeyMsg:
        // 1. Global actions (quit, approve, reject)
        // 2. Delegate to focused pane
        return m, m.Regions.FocusedPane().Update(msg)
    }
}
```

**The rule:** If an action only affects one pane, the pane handles it. If an action spans panes, AppModel orchestrates it. AppModel should never manipulate a pane's internal state (cursor position, scroll offset, selection) — it calls pane methods like `ApplyEdit()` instead.

---

### Layer 4: Services (Shared Capabilities)

Cross-cutting capabilities (clipboard, LSP, filesystem) are defined as interfaces and injected into panes via their constructors.

```go
type ClipboardService interface {
    Read() string
    Write(s string) error
}

type Services struct {
    Clipboard ClipboardService
    // Add more as needed: LSP, FileSystem, etc.
}

// Constructor injection — not globals, not a service locator
func NewEditorPane(buf *Buffer, keymap *Keymap, svc *Services) *EditorPane {
    return &EditorPane{
        buf:      buf,
        keymap:   keymap,
        services: svc,
    }
}
```

**Why interfaces:** Panes depend on `ClipboardService`, not `*SystemClipboard`. In tests, swap with a mock. On different platforms, swap implementations. The pane doesn't care.

---

### Optional: DialogModel

Modal prompts (save before quit, confirmations) are a separate model that overlays the RegionManager output:

```go
if m.Dialog.Active {
    return m.Dialog.Render(width, height)  // centered overlay
}
return m.Regions.Render()  // normal pane composition
```

The dialog captures all key input when active and emits a `DialogResultMsg` when the user makes a choice.

---

### SOLID Mapping

| Principle | How it applies |
|-----------|---------------|
| **Single Responsibility** | Each pane owns one concern. RegionManager owns layout. Services owns capabilities. AppModel owns orchestration. |
| **Open/Closed** | New panes implement `Pane` without modifying existing code. New services extend `Services`. |
| **Liskov Substitution** | Any `Pane` works in any `Region`. Any `ClipboardService` works in any pane. |
| **Interface Segregation** | `Pane` has 3 methods. `ClipboardService` has 2. No fat interfaces. |
| **Dependency Inversion** | Panes depend on `Services` (interface), not concrete implementations. |

---

### When to Use This Pattern

**Use it when:**
- Your TUI has 2+ independent UI regions
- You're adding panes over time (file tree, terminal, debugger, etc.)
- Different panes handle different message types
- You want panes to be testable in isolation

**Don't use it when:**
- Your TUI is a single full-screen view (use plain Bubble Tea)
- You have no plans for multiple panes
- The overhead of interfaces and managers isn't justified by complexity

---

### Quick Start Template

```go
// 1. Define your pane
type MyPane struct {
    width, height int
    services      *Services
}

func (p *MyPane) Update(msg tea.Msg) tea.Cmd { /* handle keys, mouse */ }
func (p *MyPane) Render() string             { /* render to string */ }
func (p *MyPane) SetSize(w, h int)           { p.width, p.height = w, h }

// 2. Wire it up
svc := NewServices()
pane1 := NewPane1(svc)
pane2 := NewPane2(svc)

rm := NewRegionManager(Horizontal)
rm.Add("main", pane1, 0.7)
rm.Add("sidebar", pane2, 0.3)

app := &AppModel{Regions: rm, Services: svc}
p := tea.NewProgram(app)
p.Run()
```

## Running

```bash
# Editor only
tui/bin/nib-code /path/to/file.go

# With AI agent
LLM_API_KEY=<key> tui/bin/nib-code /path/to/file.go

# Debug mode
tui/bin/nib-code --debug /path/to/file.go
```

## License

MIT
