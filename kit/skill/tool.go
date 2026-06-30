package skill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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
	body   string
	perm   *toolperm.Matcher
	runner dyncontext.Runner
}

// Definition returns the advertised schema: the skill's description and
// an empty-object parameter set (invoking a skill takes no arguments —
// it pulls the instructions, then the model continues).
func (t skillTool) Definition() llm.ToolDef { return t.def }

// Execute returns the skill body as the tool result. A directive-free
// body (the common case) is returned verbatim. A body with dynamic
// context (“ !`cmd` “ / fenced ` ```! `) has each directive expanded by
// [dyncontext.Expand], which runs a command only if the skill's grants
// permit it — so a prompt-only skill (deny-all matcher) never executes
// shell, and a trusted plugin's shell skill runs only its declared
// commands.
func (t skillTool) Execute(ctx context.Context, _ llm.ToolCall) upagent.ToolResult {
	body := t.body
	if dyncontext.HasDirectives(body) {
		body = dyncontext.Expand(ctx, body, t.perm, t.runner)
	}
	return upagent.ToolResult{Content: body}
}

// adaptTool builds the agent.Tool for a skill, wiring its permission
// matcher and the default shell runner so dynamic-context directives are
// gated by the skill's grants at invocation time.
func adaptTool(s Skill) skillTool {
	return skillTool{
		def: llm.ToolDef{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        ToolNamePrefix + s.Name,
				Description: s.Description,
				Parameters: llm.FunctionParams{
					Type:       "object",
					Properties: map[string]llm.FunctionParam{},
				},
			},
		},
		body:   s.Body,
		perm:   s.Permissions(),
		runner: dyncontext.NewShellRunner(0),
	}
}

// ProjectDir returns the project-local skills directory,
// <projectRoot>/.project/skills.
func ProjectDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".project", "skills")
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
	// Skipped are the skills refused in v1 (script-bearing), with the
	// reason logged. Surfaced so the refusal is visible, not silent.
	Skipped []Skill
	// Shadowed are the skills hidden by a higher-precedence same-name
	// skill (a project skill shadows a global one). Surfaced so the
	// override is visible, not silent.
	Shadowed []Skill
	// Forking are skills with `context: fork`: they run as a child agent
	// rather than injecting prompt text, so they are NOT adapted to a
	// prompt tool here. The coding layer turns each into a spawn tool.
	// Plugin forking skills appear here only when their plugin is trusted.
	Forking []Skill
}

// Discover loads skills for a project from both layers — project-local
// (<projectRoot>/.project/skills) and user-global ([GlobalDir]) — merges
// them with project shadowing global ([Merge]), and adapts the
// surviving pure-prompt skills into tools. Script-bearing skills
// ([Skill.NeedsShell]) are refused with a logged warning and recorded in
// Result.Skipped rather than adapted — executing third-party shell needs
// the (unbuilt) bash-approval surface. Replacing that branch with a
// script-skill adapter is the whole of the future extension; nothing
// else here changes.
//
// A missing directory on either layer is a no-op. A non-nil error
// reports per-skill parse failures (see [Load]); everything that did
// load is still returned, so callers may log and proceed.
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
// Shell-bearing skills ([Skill.NeedsShell]) remain refused EXCEPT a
// plugin skill whose plugin trusted reports true: trusting a plugin
// (via `/plugin trust`) opts its shell skills in. Project and global
// shell skills stay refused (nib has no per-skill shell surface for
// user-authored skills yet). Nil pluginSkills + nil trusted reproduces
// the historic [Discover] behavior.
//
// This gate decides whether a shell skill's instructions LOAD. Which
// shell commands those instructions may actually run is enforced
// per-command at invocation by the skill's [toolperm.Matcher] over its
// dynamic-context directives (see [skillTool.Execute] / [dyncontext]).
func DiscoverWithPlugins(projectRoot string, pluginSkills []PluginSkillSource, trusted TrustFunc) (Result, error) {
	var errs []error

	project, err := Load(ProjectDir(projectRoot), SourceProject)
	if err != nil {
		errs = append(errs, err)
	}

	var global []Skill
	if gdir, ok := GlobalDir(); ok {
		global, err = Load(gdir, SourceGlobal)
		if err != nil {
			errs = append(errs, err)
		}
	}

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

	var res Result
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
		if s.NeedsShell() && !shellTrusted(s, trusted) {
			res.Skipped = append(res.Skipped, s)
			slog.Warn("skill: refused script-bearing skill (untrusted or no shell surface)",
				"skill", s.Name, "source", s.Source, "plugin", s.PluginID,
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

// shellTrusted reports whether a shell-bearing skill may load: only a
// plugin skill with a non-empty id whose plugin the user has trusted. The
// empty-id guard is defense-in-depth against an unidentified plugin
// collapsing onto trusted(""). Project/global shell skills are never
// auto-trusted here.
func shellTrusted(s Skill, trusted TrustFunc) bool {
	return s.Source == SourcePlugin && s.PluginID != "" && trusted != nil && trusted(s.PluginID)
}
