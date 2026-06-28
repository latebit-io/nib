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
)

// ToolNamePrefix namespaces skill tool names so a skill can never
// shadow (and be silently dropped by) a built-in tool — built-ins are
// registered first and win on name collision. Exported so the prompt
// layer can detect skill tools without re-deriving the convention.
const ToolNamePrefix = "skill_"

// skillTool adapts a pure-prompt [Skill] into an [agent.Tool]. The
// description is advertised in the tool list; Execute returns the body
// so the instructions load only when the model invokes the skill.
type skillTool struct {
	def  llm.ToolDef
	body string
}

// Definition returns the advertised schema: the skill's description and
// an empty-object parameter set (invoking a skill takes no arguments —
// it pulls the instructions, then the model continues).
func (t skillTool) Definition() llm.ToolDef { return t.def }

// Execute returns the skill body verbatim as the tool result. It never
// fails and ignores the call arguments — the skill is pure instruction
// text, with no side effects.
func (t skillTool) Execute(_ context.Context, _ llm.ToolCall) upagent.ToolResult {
	return upagent.ToolResult{Content: t.body}
}

// adaptTool builds the agent.Tool for a pure-prompt skill.
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
		body: s.Body,
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
	return DiscoverWithPlugins(projectRoot, nil)
}

// DiscoverWithPlugins extends [Discover] with skills imported from
// managed plugins. pluginSkillDirs are the converted "skills" roots of
// the enabled plugins (each a directory of <name>/SKILL.md). Plugin
// skills join as the lowest-precedence layer ([SourcePlugin]): a
// same-named project or global skill shadows them, so a user's own
// skills always win over a third-party import. A nil/empty slice makes
// this identical to the historic [Discover] behavior.
func DiscoverWithPlugins(projectRoot string, pluginSkillDirs []string) (Result, error) {
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
	for _, dir := range pluginSkillDirs {
		ps, perr := Load(dir, SourcePlugin)
		if perr != nil {
			errs = append(errs, perr)
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
		if s.NeedsShell() {
			res.Skipped = append(res.Skipped, s)
			slog.Warn("skill: refused script-bearing skill (shell execution needs bash approval, not yet available)",
				"skill", s.Name, "source", s.Source, "path", s.Path, "allowed_tools", s.AllowedTools)
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
