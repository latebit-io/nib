package skill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/dyncontext"
	"github.com/latebit-io/nib/kit/toolperm"
)

// ToolNamePrefix namespaces skill tool names so a skill can never
// shadow (and be silently dropped by) a built-in tool — built-ins are
// registered first and win on name collision. Exported so the prompt
// layer can detect skill tools without re-deriving the convention.
const ToolNamePrefix = "skill_"

// skillTool adapts a [Skill] into an [agent.Tool]. The description is
// advertised in the tool list; Execute returns the body so the
// instructions load only when the model invokes the skill, expanding any
// dynamic-context directives gated by the skill's tool grants.
type skillTool struct {
	def    llm.ToolDef
	skill  Skill
	perm   *toolperm.Matcher
	runner dyncontext.Runner
}

// Definition returns the advertised schema: the skill's description and
// one optional argument, for reading a bundled file instead of the body.
func (t skillTool) Definition() llm.ToolDef { return t.def }

// Execute returns the skill body, then its [Skill.ResourceNote]; with a
// file argument it returns that bundled file instead. A dynamic-context
// directive in the body runs only if the skill's grants permit it.
func (t skillTool) Execute(ctx context.Context, call llm.ToolCall) upagent.ToolResult {
	args, err := llm.DecodeArguments[resourceArgs](t.def.Function.Parameters, call.Function.Arguments)
	if err != nil {
		return upagent.ToolResult{Content: err.Error(), IsError: true}
	}
	if args.File != "" {
		content, err := t.skill.ReadResource(args.File)
		if err != nil {
			return upagent.ToolResult{Content: "cannot read bundled file: " + err.Error(), IsError: true}
		}
		return upagent.ToolResult{Content: content}
	}
	body := t.skill.Body
	if dyncontext.HasDirectives(body) {
		body = dyncontext.Expand(ctx, body, t.perm, t.runner)
	}
	return upagent.ToolResult{Content: body + t.skill.ResourceNote(t.def.Function.Name)}
}

// BindShell implements [kit.ShellBinder]: it returns a copy of the tool
// whose dynamic-context directives execute via r instead of the default
// [dyncontext.ShellRunner]. The coding agent binds a runner over its
// own bash tool so directives pass the same per-command approval,
// allowlist, and subagent grant gates as a model-issued bash call. Value
// receiver: the receiver is left untouched, so a discovered tool shared
// between agents binds independently for each.
func (t skillTool) BindShell(r dyncontext.Runner) upagent.Tool {
	t.runner = r
	return t
}

// adaptTool builds the agent.Tool for a skill, wiring its permission
// matcher and the default shell runner so dynamic-context directives are
// gated by the skill's grants at invocation time. Agents with a shell
// gate rebind the runner via [skillTool.BindShell].
func adaptTool(s Skill) skillTool {
	return skillTool{
		def: llm.ToolDef{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        ToolNamePrefix + s.Name,
				Description: s.Description,
				Parameters: llm.FunctionParams{
					Type: "object",
					Properties: map[string]llm.FunctionParam{"file": {
						Type:        "string",
						Description: "Bundled file to read instead of the instructions, as listed after them. Omit to load the skill.",
					}},
				},
			},
		},
		skill:  s,
		perm:   s.Permissions(),
		runner: dyncontext.NewShellRunner(0),
	}
}

// ProjectDir returns the project-local skills directory,
// <projectRoot>/.project/skills.
func ProjectDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".project", "skills")
}

// sharedDir is the skills directory, relative to a project root or the
// user's home, that Agent Skills clients share (agentskills.io).
var sharedDir = filepath.Join(".agents", "skills")

// SharedProjectDir returns the cross-client project skills directory,
// <projectRoot>/.agents/skills. [ProjectDir] shadows it by name.
func SharedProjectDir(projectRoot string) string {
	return filepath.Join(projectRoot, sharedDir)
}

// SharedGlobalDir returns the cross-client user skills directory,
// ~/.agents/skills, and true, or ("", false) when the home directory
// cannot be resolved. [GlobalDir] shadows it by name.
func SharedGlobalDir() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("skill: cannot resolve home dir; skipping ~/.agents/skills", "err", err)
		return "", false
	}
	return filepath.Join(home, sharedDir), true
}

// GlobalDir returns the user-global skills directory and true, or
// ("", false) when it cannot be resolved (no UserConfigDir on this
// platform). The default is <UserConfigDir>/<brand.ConfigDirName>/skills,
// mirroring the global-commands convention; [brand.EnvKeyGlobalSkillsDir]
// overrides it when set non-empty.
func GlobalDir() (string, bool) {
	if dir := os.Getenv(brand.EnvKeyGlobalSkillsDir); dir != "" {
		return dir, true
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		slog.Warn("skill: cannot resolve user config dir; skipping global skills layer", "err", err)
		return "", false
	}
	return filepath.Join(cfg, brand.ConfigDirName, "skills"), true
}

// Result is the outcome of [Discover]: the adapted tools plus the skills
// that loaded, were refused, or were shadowed, so the wiring site can
// surface each (e.g. via --plugins). Loaded[i] corresponds to Tools[i].
type Result struct {
	// Tools are the agent-compatible adapters for supported skills,
	// ready to pass as extra tools at agent construction.
	Tools []upagent.Tool
	// Loaded are the skills adapted into Tools, parallel to Tools.
	Loaded []Skill
	// Skipped are the plugin shell/fork skills refused because their
	// plugin is untrusted, with the reason logged. Surfaced so the
	// refusal is visible, not silent.
	Skipped []Skill
	// Shadowed are the skills hidden by a higher-precedence same-name
	// skill (a project skill shadows a global one). Surfaced so the
	// override is visible, not silent.
	Shadowed []Skill
	// Refused are the project skills whose link resolves outside the
	// project. A cloned repository may not pull in a directory beyond it.
	Refused []Skill
	// Forking are skills with `context: fork`: they run as a child agent
	// rather than injecting prompt text, so they are NOT adapted to a
	// prompt tool here. The coding layer turns each into a spawn tool.
	// Plugin forking skills appear here only when their plugin is trusted.
	Forking []Skill
}

// Discover loads project skills ([ProjectDir], [SharedProjectDir]) and
// user skills ([GlobalDir], [SharedGlobalDir]), merges them ([Merge]), and
// adapts the survivors. Skills that parsed are returned even with an error.
func Discover(projectRoot string) (Result, error) {
	return DiscoverWithPlugins(projectRoot, nil, nil)
}

// PluginSkillSource locates one enabled plugin's converted skills
// directory together with the plugin id the trust gate keys on.
type PluginSkillSource struct {
	// ID is the managed-plugin id (stamped onto each loaded skill).
	ID string
	// Dir is the converted skills root (<converted>/skills).
	Dir string
}

// TrustFunc reports whether a managed plugin is trusted to run shell.
// Supplied by the wiring layer (backed by the plugin store's persisted
// trust grant). A nil TrustFunc means no plugin is trusted.
type TrustFunc func(pluginID string) bool

// DiscoverWithPlugins extends [Discover] with skills imported from
// managed plugins. Each [PluginSkillSource] contributes its converted
// skills as the lowest-precedence layer ([SourcePlugin]); a same-named
// project or global skill shadows them, so a user's own skills always
// win over a third-party import.
//
// A plugin's shell-bearing skill ([Skill.NeedsShell]) loads only when
// trusted reports true for its plugin: trusting a plugin (via `/plugin
// trust`) opts its shell skills in, because third-party provenance needs
// a vouch before its instructions enter the prompt at all. Project and
// global shell skills load unconditionally — the per-command bash
// approval surface is the backstop for the shell they request. Nil
// pluginSkills + nil trusted reproduces the [Discover] behavior.
//
// This gate decides whether a shell skill's instructions LOAD. Which
// shell commands those instructions may actually run is enforced
// per-command at invocation: the skill's [toolperm.Matcher] over its
// dynamic-context directives (see [skillTool.Execute] / [dyncontext]),
// then the hosting agent's bash gate once it rebinds the runner.
func DiscoverWithPlugins(projectRoot string, pluginSkills []PluginSkillSource, trusted TrustFunc) (Result, error) {
	var errs []error

	// Within a scope nib's own directory comes first, so it wins a name
	// clash with the cross-client .agents/skills directory.
	var globalDirs []string
	if dir, ok := GlobalDir(); ok {
		globalDirs = append(globalDirs, dir)
	}
	if dir, ok := SharedGlobalDir(); ok {
		globalDirs = append(globalDirs, dir)
	}
	load := func(source Source, dirs ...string) []Skill {
		var layer []Skill
		for _, dir := range dirs {
			ss, err := Load(dir, source)
			if err != nil {
				errs = append(errs, err)
			}
			layer = append(layer, ss...)
		}
		return layer
	}
	project := load(SourceProject, ProjectDir(projectRoot), SharedProjectDir(projectRoot))
	// A cloned repository is untrusted: a project skill link may not name
	// a directory outside it. User-scope links may point anywhere.
	var res Result
	root, rootErr := realDir(projectRoot)
	project = slices.DeleteFunc(project, func(s Skill) bool {
		rel, err := filepath.Rel(root, s.Dir)
		if rootErr == nil && err == nil && filepath.IsLocal(rel) {
			return false
		}
		res.Refused = append(res.Refused, s)
		slog.Warn("skill: refused project skill linked outside the project",
			"skill", s.Name, "path", s.Path, "target", s.Dir)
		return true
	})
	global := load(SourceGlobal, globalDirs...)

	var plugin []Skill
	for _, src := range pluginSkills {
		// An empty id cannot be a trust key — every trust lookup would
		// collapse onto trusted(""). Skip such a source rather than load
		// skills whose provenance cannot be verified.
		if src.ID == "" {
			slog.Warn("skill: skipping plugin skills source with empty id", "dir", src.Dir)
			continue
		}
		ps, perr := Load(src.Dir, SourcePlugin)
		if perr != nil {
			errs = append(errs, perr)
		}
		for i := range ps {
			ps[i].PluginID = src.ID // stamp provenance for the trust gate
		}
		plugin = append(plugin, ps...)
	}

	// Precedence high→low: project shadows global shadows plugin. Pass in
	// that order so earlier layers' names win.
	winners, shadowed := Merge(project, global, plugin)

	res.Shadowed = shadowed
	for _, s := range shadowed {
		slog.Info("skill: shadowed by higher-precedence skill",
			"skill", s.Name, "source", s.Source, "path", s.Path)
	}
	for _, s := range winners {
		// Forking skills run as a child agent; they are routed to the
		// coding layer rather than adapted to a prompt tool. A plugin
		// forking skill requires trust — it spawns an agent (tools/edits),
		// so it is at least as powerful as a shell skill.
		if s.IsFork() {
			if s.Source == SourcePlugin && !shellTrusted(s, trusted) {
				res.Skipped = append(res.Skipped, s)
				slog.Warn("skill: refused untrusted plugin fork skill (run /plugin trust to enable)",
					"skill", s.Name, "plugin", s.PluginID, "path", s.Path)
				continue
			}
			res.Forking = append(res.Forking, s)
			continue
		}
		if s.NeedsShell() && s.Source == SourcePlugin && !shellTrusted(s, trusted) {
			res.Skipped = append(res.Skipped, s)
			slog.Warn("skill: refused untrusted plugin shell skill (run /plugin trust to enable)",
				"skill", s.Name, "plugin", s.PluginID,
				"path", s.Path, "allowed_tools", s.AllowedTools)
			continue
		}
		res.Tools = append(res.Tools, adaptTool(s))
		res.Loaded = append(res.Loaded, s)
	}

	if len(errs) > 0 {
		return res, fmt.Errorf("load skills: %w", errors.Join(errs...))
	}
	return res, nil
}

// shellTrusted reports whether a plugin's shell-bearing (or forking)
// skill may load: the plugin id is non-empty and the user has trusted
// it. The empty-id guard is defense-in-depth against an unidentified
// plugin collapsing onto trusted(""). Callers gate on Source themselves;
// project/global skills never reach this predicate.
func shellTrusted(s Skill, trusted TrustFunc) bool {
	return s.Source == SourcePlugin && s.PluginID != "" && trusted != nil && trusted(s.PluginID)
}
