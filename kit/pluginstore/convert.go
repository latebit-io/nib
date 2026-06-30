package pluginstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/latebit-io/nib/kit/agentdef"
	"github.com/latebit-io/nib/kit/frontmatter"
	"github.com/latebit-io/nib/kit/toolperm"
	"gopkg.in/yaml.v3"
)

// ConvertReport summarizes what a CC→nib conversion produced and, just
// as importantly, what it could not yet honor. The unsupported list is
// the honest record that keeps import from silently dropping capability:
// each entry names a component and why nib cannot run it at this
// milestone.
type ConvertReport struct {
	// Commands are the converted command names (namespaced).
	Commands []string
	// Skills are the converted skill names (namespaced).
	Skills []string
	// Agents are the converted subagent definition names (namespaced).
	Agents []string
	// MCPServers are the converted MCP server names.
	MCPServers []string
	// Unsupported lists components/fields not yet honored.
	Unsupported []Unsupported
}

// Unsupported is one component or feature the converter recognized but
// could not translate at this milestone.
type Unsupported struct {
	// Kind classifies the gap (e.g. "agent", "hooks", "skill-shell",
	// "mcp-transport", "var").
	Kind string
	// Name identifies the specific item (component name, var, …).
	Name string
	// Reason is a short human explanation, typically naming the
	// milestone that will close the gap.
	Reason string
}

// add appends an unsupported entry.
func (r *ConvertReport) add(kind, name, reason string) {
	r.Unsupported = append(r.Unsupported, Unsupported{Kind: kind, Name: name, Reason: reason})
}

// Convert reads the raw CC plugin tree at src (default component
// locations) and writes nib-native artifacts into dst, returning a
// report. It is pure file transformation: no network, no execution.
//
// M1 converts commands, prompt-only skills, and stdio MCP servers.
// Agents, hooks, shell-bearing skills, and non-stdio MCP transports are
// recorded in the report's Unsupported list for the milestone that will
// handle them, not silently dropped.
func Convert(src, dst string, m Manifest, vars Vars) (ConvertReport, error) {
	var report ConvertReport
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return report, fmt.Errorf("pluginstore: create converted dir: %w", err)
	}
	if err := convertMCP(src, dst, vars, &report); err != nil {
		return report, err
	}
	if err := convertCommands(src, dst, m.Name, vars, &report); err != nil {
		return report, err
	}
	if err := convertSkills(src, dst, m.Name, vars, &report); err != nil {
		return report, err
	}
	if err := convertAgents(src, dst, m.Name, vars, &report); err != nil {
		return report, err
	}
	noteDeferredComponents(src, &report)
	slices.Sort(report.Commands)
	slices.Sort(report.Skills)
	slices.Sort(report.Agents)
	slices.Sort(report.MCPServers)
	return report, nil
}

// --- MCP ---

type mcpFileIn struct {
	Servers map[string]json.RawMessage `json:"mcpServers"`
}

type mcpServerIn struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
}

type mcpServerOut struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type mcpFileOut struct {
	Servers map[string]mcpServerOut `json:"mcpServers"`
}

// convertMCP reads <src>/.mcp.json, freezes import-time variables, drops
// non-stdio transports (with a report note), and writes the nib-shaped
// <dst>/.mcp.json.
func convertMCP(src, dst string, vars Vars, report *ConvertReport) error {
	path := filepath.Join(src, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pluginstore: read %s: %w", path, err)
	}
	var in mcpFileIn
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("pluginstore: parse %s: %w", path, err)
	}

	out := mcpFileOut{Servers: map[string]mcpServerOut{}}
	for name, raw := range in.Servers {
		var s mcpServerIn
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("pluginstore: parse mcp server %q: %w", name, err)
		}
		if t := strings.ToLower(s.Type); t != "" && t != "stdio" {
			report.add("mcp-transport", name, fmt.Sprintf("%q transport unsupported (stdio only; M5)", s.Type))
			continue
		}
		if s.Cwd != "" {
			report.add("mcp-cwd", name, "cwd field ignored (M5)")
		}
		cmd, un := expandVars(s.Command, vars)
		noteVars(report, un)
		args := make([]string, len(s.Args))
		for i, a := range s.Args {
			ea, u := expandVars(a, vars)
			noteVars(report, u)
			args[i] = ea
		}
		var env map[string]string
		if len(s.Env) > 0 {
			env = make(map[string]string, len(s.Env))
			for k, v := range s.Env {
				ev, u := expandVars(v, vars)
				noteVars(report, u)
				env[k] = ev
			}
		}
		out.Servers[name] = mcpServerOut{Command: cmd, Args: args, Env: env}
		report.MCPServers = append(report.MCPServers, name)
	}
	if len(out.Servers) == 0 {
		return nil
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("pluginstore: marshal mcp: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dst, ".mcp.json"), body, 0o644); err != nil {
		return fmt.Errorf("pluginstore: write converted mcp: %w", err)
	}
	return nil
}

// --- commands ---

type nibCommandMeta struct {
	Name            string   `yaml:"name"`
	Description     string   `yaml:"description,omitempty"`
	AllowedTools    []string `yaml:"allowed-tools,omitempty"`
	DisallowedTools []string `yaml:"disallowed-tools,omitempty"`
}

// convertCommands translates <src>/commands/*.md into nib command files
// under <dst>/commands/, namespacing each command name with the plugin
// name. CC and nib share the $ARGUMENTS/$1 substitution syntax, so only
// the frontmatter is rewritten; import-time vars in the body are frozen.
func convertCommands(src, dst, pluginName string, vars Vars, report *ConvertReport) error {
	dir := filepath.Join(src, "commands")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pluginstore: read commands dir: %w", err)
	}
	outDir := filepath.Join(dst, "commands")
	seen := map[string]string{} // namespaced name → source filename
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("pluginstore: read command %s: %w", e.Name(), err)
		}
		fm := map[string]any{}
		body, err := frontmatter.Split(data, &fm)
		if err != nil {
			report.add("command", e.Name(), "malformed frontmatter; skipped")
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".md")
		if n, ok := fm["name"].(string); ok && n != "" {
			base = n
		}
		name := namespacedName(pluginName, base)
		if name == "" {
			report.add("command", e.Name(), "could not derive a valid name; skipped")
			continue
		}
		if prev, dup := seen[name]; dup {
			report.add("command", e.Name(), fmt.Sprintf("name %q collides with %s after sanitization; skipped", name, prev))
			continue
		}
		seen[name] = e.Name()

		allow, deny, gErr := parseGrants(fm)
		if gErr != nil {
			report.add("command", e.Name(), "malformed tool grant; skipped: "+gErr.Error())
			continue
		}
		if toolperm.AnyShell(allow) {
			report.add("command-tools", name, "carries shell grants; per-command enforcement lands at M2")
		}
		expBody, un := expandVars(body, vars)
		noteVars(report, un)

		meta := nibCommandMeta{
			Name:            name,
			Description:     stringField(fm, "description"),
			AllowedTools:    toolperm.Strings(allow),
			DisallowedTools: toolperm.Strings(deny),
		}
		if err := writeMarkdown(filepath.Join(outDir, name+".md"), meta, expBody); err != nil {
			return err
		}
		report.Commands = append(report.Commands, name)
	}
	return nil
}

// --- skills ---

type nibSkillMeta struct {
	Name            string   `yaml:"name"`
	Description     string   `yaml:"description,omitempty"`
	AllowedTools    []string `yaml:"allowed-tools,omitempty"`
	DisallowedTools []string `yaml:"disallowed-tools,omitempty"`
	Context         string   `yaml:"context,omitempty"`
	Agent           string   `yaml:"agent,omitempty"`
}

// convertSkills translates <src>/skills/<n>/SKILL.md into nib skills
// under <dst>/skills/<ns>/. Shell-bearing skills are recorded as
// deferred (nib refuses script skills until the M2 trust+shell layer)
// and not copied; prompt-only skills are copied whole (reference files
// included) with a namespaced, var-frozen SKILL.md.
func convertSkills(src, dst, pluginName string, vars Vars, report *ConvertReport) error {
	dir := filepath.Join(src, "skills")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pluginstore: read skills dir: %w", err)
	}
	seen := map[string]string{} // namespaced name → source skill dir
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillSrc := filepath.Join(dir, e.Name())
		mdPath := filepath.Join(skillSrc, "SKILL.md")
		data, err := os.ReadFile(mdPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // not a skill dir
			}
			return fmt.Errorf("pluginstore: read skill %s: %w", e.Name(), err)
		}
		fm := map[string]any{}
		body, err := frontmatter.Split(data, &fm)
		if err != nil {
			report.add("skill", e.Name(), "malformed frontmatter; skipped")
			continue
		}
		base := e.Name()
		if n, ok := fm["name"].(string); ok && n != "" {
			base = n
		}
		name := namespacedName(pluginName, base)
		if name == "" {
			report.add("skill", e.Name(), "could not derive a valid name; skipped")
			continue
		}
		if prev, dup := seen[name]; dup {
			report.add("skill", e.Name(), fmt.Sprintf("name %q collides with %s after sanitization; skipped", name, prev))
			continue
		}
		seen[name] = e.Name()

		allow, deny, gErr := parseGrants(fm)
		if gErr != nil {
			report.add("skill", e.Name(), "malformed tool grant; skipped: "+gErr.Error())
			continue
		}
		// Normalize + validate `context` so the converted SKILL.md always
		// satisfies the loader (which accepts only ""/"fork"). An invalid
		// value would convert "successfully" then fail every later load.
		ctxMode := strings.ToLower(strings.TrimSpace(stringField(fm, "context")))
		if ctxMode != "" && ctxMode != "fork" {
			report.add("skill", e.Name(), fmt.Sprintf("invalid context %q; skipped", ctxMode))
			continue
		}
		// `agent:` references are not yet supported and the loader rejects
		// them; drop (and report) rather than emit an unloadable artifact.
		if strings.TrimSpace(stringField(fm, "agent")) != "" {
			report.add("skill", name, "agent: reference dropped (not yet supported)")
		}
		// Shell-bearing skills are converted WITH their grants preserved,
		// but they will not execute until the trust + shell-execution layer
		// lands: the skill loader still refuses script-bearing skills. Note
		// it so the user knows the artifact exists but is inert for now.
		if toolperm.AnyShell(allow) {
			report.add("skill-shell", name, "shell grant carried; runs only once the plugin is trusted (M2)")
		}
		skillDst := filepath.Join(dst, "skills", name)
		if err := copyTree(skillSrc, skillDst); err != nil {
			return fmt.Errorf("pluginstore: copy skill %s: %w", name, err)
		}
		expBody, un := expandVars(body, vars)
		noteVars(report, un)
		meta := nibSkillMeta{
			Name:            name,
			Description:     stringField(fm, "description"),
			AllowedTools:    toolperm.Strings(allow),
			DisallowedTools: toolperm.Strings(deny),
			Context:         ctxMode, // normalized; agent dropped (unsupported)
		}
		if err := writeMarkdown(filepath.Join(skillDst, "SKILL.md"), meta, expBody); err != nil {
			return err
		}
		report.Skills = append(report.Skills, name)
	}
	return nil
}

// nibAgentMeta is the frontmatter emitted for a converted subagent
// definition. Mirrors the CC agent keys nib's loader reads.
type nibAgentMeta struct {
	Name            string   `yaml:"name"`
	Description     string   `yaml:"description,omitempty"`
	Tools           []string `yaml:"tools,omitempty"`
	DisallowedTools []string `yaml:"disallowedTools,omitempty"`
	Model           string   `yaml:"model,omitempty"`
	Effort          string   `yaml:"effort,omitempty"`
	MaxTurns        int      `yaml:"maxTurns,omitempty"`
	Skills          []string `yaml:"skills,omitempty"`
	Isolation       string   `yaml:"isolation,omitempty"`
}

// convertAgents translates <src>/agents/*.md into nib-native subagent
// definitions under <dst>/agents/, namespacing each name and preserving
// its grants/config. The definitions are inert until the subagent engine
// lands, but converting them now means a re-sync lights them up rather
// than requiring a re-import. A malformed definition is reported and
// skipped, not silently dropped.
func convertAgents(src, dst, pluginName string, vars Vars, report *ConvertReport) error {
	dir := filepath.Join(src, "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pluginstore: read agents dir: %w", err)
	}
	seen := map[string]string{} // namespaced name → source filename
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		def, perr := agentdef.Parse(filepath.Join(dir, e.Name()), agentdef.SourcePlugin)
		if perr != nil {
			report.add("agent", e.Name(), "malformed agent definition; skipped: "+perr.Error())
			continue
		}
		name := namespacedName(pluginName, def.Name)
		if name == "" {
			report.add("agent", e.Name(), "could not derive a valid name; skipped")
			continue
		}
		if prev, dup := seen[name]; dup {
			report.add("agent", e.Name(), fmt.Sprintf("name %q collides with %s after sanitization; skipped", name, prev))
			continue
		}
		seen[name] = e.Name()
		prompt, un := expandVars(def.SystemPrompt, vars)
		noteVars(report, un)
		meta := nibAgentMeta{
			Name:            name,
			Description:     def.Description,
			Tools:           def.AllowedTools,
			DisallowedTools: def.DisallowedTools,
			Model:           def.Model,
			Effort:          def.Effort,
			MaxTurns:        def.MaxTurns,
			// The agent's preloaded skills are plugin-local; rewrite each
			// reference to the namespaced name the skill converter emitted,
			// so the future subagent loader binds to this plugin's skill
			// rather than missing it or matching another layer's skill.
			Skills:    namespaceRefs(pluginName, def.Skills),
			Isolation: string(def.Isolation),
		}
		if err := writeMarkdown(filepath.Join(dst, "agents", name+".md"), meta, prompt); err != nil {
			return err
		}
		report.Agents = append(report.Agents, name)
		report.add("agent", name, "converted; runs once the subagent engine lands (M4)")
	}
	return nil
}

// noteDeferredComponents records components present in the plugin but not
// yet runnable, pointing at the milestone that will handle them.
func noteDeferredComponents(src string, report *ConvertReport) {
	if _, err := os.Stat(filepath.Join(src, "hooks", "hooks.json")); err == nil {
		report.add("hooks", "hooks.json", "requires hooks engine (M3)")
	}
}

// --- helpers ---

// nameSanitizer collapses runs of characters outside nib's name alphabet
// (lowercase alnum, underscore, dash) into a single dash.
var nameSanitizer = regexp.MustCompile(`[^a-z0-9_-]+`)

// namespaceRefs maps a list of plugin-local skill references through
// [namespacedName] so they match the converter's emitted skill names.
// Empty results (a ref that sanitizes to nothing) are dropped.
func namespaceRefs(pluginName string, refs []string) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if n := namespacedName(pluginName, r); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// namespacedName builds a nib-valid, plugin-namespaced command/skill
// name: "<plugin>-<base>", lowercased and sanitized. Returns "" when
// nothing valid survives.
func namespacedName(pluginName, base string) string {
	raw := strings.ToLower(pluginName + "-" + base)
	n := nameSanitizer.ReplaceAllString(raw, "-")
	return strings.Trim(n, "-_")
}

// parseGrants extracts the allowed-tools and disallowed-tools grants from
// a skill/command frontmatter map. A malformed grant is an error, not a
// silent drop: converting an artifact with a broad allow but a dropped
// (malformed) deny would weaken the imported policy. Callers skip and
// report the artifact on error.
func parseGrants(fm map[string]any) (allow, deny []toolperm.Rule, err error) {
	allow, aerr := toolperm.ParseField(fm["allowed-tools"])
	if aerr != nil {
		return nil, nil, fmt.Errorf("allowed-tools: %w", aerr)
	}
	deny, derr := toolperm.ParseField(fm["disallowed-tools"])
	if derr != nil {
		return nil, nil, fmt.Errorf("disallowed-tools: %w", derr)
	}
	return allow, deny, nil
}

// stringField reads a string frontmatter field, empty when absent.
func stringField(fm map[string]any, key string) string {
	if s, ok := fm[key].(string); ok {
		return s
	}
	return ""
}

// noteVars records unresolved runtime variables once each.
func noteVars(report *ConvertReport, names []string) {
	for _, n := range names {
		already := slices.ContainsFunc(report.Unsupported, func(u Unsupported) bool {
			return u.Kind == "var" && u.Name == n
		})
		if !already {
			report.add("var", n, "runtime variable left unexpanded (supply at M5)")
		}
	}
}

// writeMarkdown writes a nib markdown artifact with YAML frontmatter and
// a body, creating parent directories.
func writeMarkdown(path string, meta any, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	y, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("pluginstore: marshal frontmatter: %w", err)
	}
	content := "---\n" + string(y) + "---\n\n" + strings.TrimLeft(body, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("pluginstore: write %s: %w", path, err)
	}
	return nil
}
