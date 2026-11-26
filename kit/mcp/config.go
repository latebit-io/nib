package mcp

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/latebit-io/nib/kit/internal/filecap"
)

// ProjectConfigName is the file a project names its servers in, the
// same file and format other agents read.
const ProjectConfigName = ".mcp.json"

// maxConfigBytes caps one config file. Real ones are a few hundred bytes.
const maxConfigBytes = 256 << 10

// Origin says who supplied a server definition, which decides whether
// it may start without being asked for.
type Origin string

const (
	// OriginUser is the person's own list, from the environment.
	OriginUser Origin = "user"
	// OriginPlugin is an installed plugin's config file.
	OriginPlugin Origin = "plugin"
	// OriginProject is the repository's [ProjectConfigName].
	OriginProject Origin = "project"
)

// Server is one stdio server definition.
type Server struct {
	// Name identifies the server and prefixes its tools' names.
	Name string
	// Command is the program, optionally followed by arguments.
	Command string
	// Args are appended after any arguments in Command.
	Args []string
	// Env is added to the inherited environment.
	Env map[string]string
	// Origin says who supplied the definition.
	Origin Origin
	// Source is the file or environment variable it came from.
	Source string
}

// ErrUntrusted is in the chain of [Refusal.Err] for a project server
// that only lacks the person's approval, so a host can say how to give it.
var ErrUntrusted = errors.New("this project's servers are not trusted")

// Refusal is a definition that will not be started, and why.
type Refusal struct {
	Server Server
	Err    error
}

// Config is what [Discover] found: the servers that may start, sorted
// by name so the tool list is stable across runs, and those refused.
type Config struct {
	Servers []Server
	Refused []Refusal
}

// ConfigSource is one plugin's config file. Prefix (the plugin id) is
// joined to each server name, so two plugins cannot collide.
type ConfigSource struct {
	Prefix string
	Path   string
}

// DiscoverOptions says where [Discover] looks.
type DiscoverOptions struct {
	// ProjectDir is the directory whose [ProjectConfigName] is read.
	// Empty skips it.
	ProjectDir string
	// TrustProject lets the project's servers start. Off by default: a
	// cloned repository must not start a process nobody asked for.
	TrustProject bool
	// User are the person's own servers, for example from [ParseSpec]
	// on an environment variable. They win a name over the others.
	User []Server
	// Plugins are config files contributed by installed plugins.
	Plugins []ConfigSource
}

// Discover collects server definitions from the person's list, plugins
// and the project, in that order; a name already taken is refused. A
// file that cannot be used is reported in err and the rest still load.
func Discover(opts DiscoverOptions) (Config, error) {
	found := slices.Clone(opts.User)
	var errs []error
	for _, src := range opts.Plugins {
		servers, err := readConfig(src.Path, OriginPlugin, src.Prefix+"__")
		errs = append(errs, err)
		found = append(found, servers...)
	}
	if opts.ProjectDir != "" {
		servers, err := readConfig(filepath.Join(opts.ProjectDir, ProjectConfigName), OriginProject, "")
		errs = append(errs, err)
		found = append(found, servers...)
	}

	var cfg Config
	taken := map[string]Origin{}
	for _, s := range found {
		if err := refusal(s, taken, opts.TrustProject); err != nil {
			cfg.Refused = append(cfg.Refused, Refusal{Server: s, Err: err})
			continue
		}
		taken[s.Name] = s.Origin
		cfg.Servers = append(cfg.Servers, s)
	}
	slices.SortFunc(cfg.Servers, func(a, b Server) int { return strings.Compare(a.Name, b.Name) })
	return cfg, errors.Join(errs...)
}

// refusal returns why s may not start, or nil when it may.
func refusal(s Server, taken map[string]Origin, trustProject bool) error {
	if strings.TrimSpace(s.Command) == "" {
		return errors.New("no command")
	}
	if key := deniedEnvKey(s.Env); key != "" {
		return fmt.Errorf("sets %s, which changes what code the server loads", key)
	}
	if s.Origin == OriginProject && !trustProject {
		return ErrUntrusted
	}
	if origin, ok := taken[s.Name]; ok {
		return fmt.Errorf("the name is already used by a %s server", origin)
	}
	return nil
}

// readConfig parses one config file. A missing file is not an error.
func readConfig(path string, origin Origin, prefix string) ([]Server, error) {
	var file struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if _, err := filecap.ReadJSON(path, maxConfigBytes, "MCP config", &file); err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	servers := make([]Server, 0, len(file.Servers))
	// Sorted: which of two colliding names wins must not depend on map order.
	for _, name := range slices.Sorted(maps.Keys(file.Servers)) {
		def := file.Servers[name]
		servers = append(servers, Server{
			Name: prefix + name, Command: def.Command, Args: def.Args, Env: def.Env,
			Origin: origin, Source: path,
		})
	}
	return servers, nil
}

// ParseSpec parses the person's own list, "name=command args" entries
// separated by semicolons, as a binary reads it from its environment.
// source names where the list came from. A bad entry does not stop the rest.
func ParseSpec(spec, source string) ([]Server, error) {
	var servers []Server
	var errs []error
	for entry := range strings.SplitSeq(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, command, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(command) == "" {
			errs = append(errs, fmt.Errorf("mcp: %s: entry %q is not name=command", source, entry))
			continue
		}
		servers = append(servers, Server{
			Name: strings.TrimSpace(name), Command: command, Origin: OriginUser, Source: source,
		})
	}
	return servers, errors.Join(errs...)
}

// deniedEnv are variables that make a process load code from somewhere
// else. A definition's command is read before it is trusted; its
// environment usually is not, so these may not be set there.
var deniedEnv = []string{
	"PATH", "NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP",
	"RUBYOPT", "RUBYLIB", "PERL5OPT", "PERL5LIB", "CLASSPATH", "JAVA_TOOL_OPTIONS",
	"BASH_ENV", "ENV", "GOFLAGS",
}

// deniedEnvPrefixes cover the dynamic loader's whole family of variables.
var deniedEnvPrefixes = []string{"LD_", "DYLD_"}

// deniedEnvKey returns the first key in env a definition may not set,
// in sorted order, or "".
func deniedEnvKey(env map[string]string) string {
	for _, key := range slices.Sorted(maps.Keys(env)) {
		// Windows matches variable names without regard to case.
		upper := strings.ToUpper(key)
		if slices.Contains(deniedEnv, upper) {
			return key
		}
		for _, prefix := range deniedEnvPrefixes {
			if strings.HasPrefix(upper, prefix) {
				return key
			}
		}
	}
	return ""
}
