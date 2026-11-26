package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
	// Command is the program to run, taken whole: a path with spaces
	// in it is one program, not a program and its arguments.
	Command string
	// Args are the program's arguments.
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
	// Trust, when set, lets them start if the person approved the
	// config file. It is asked about the bytes that are then parsed.
	Trust *TrustStore
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
		data, err := filecap.Read(src.Path, maxConfigBytes, "MCP config")
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("mcp: %w", err))
			continue
		}
		servers, err := parseConfig(data, src.Path, OriginPlugin, src.Prefix+"__")
		errs = append(errs, err)
		found = append(found, servers...)
	}
	trusted := opts.TrustProject
	if opts.ProjectDir != "" {
		project, err := readProject(opts.ProjectDir)
		errs = append(errs, err)
		if project.found {
			// Approval and parsing use the same bytes: a file swapped
			// after the check cannot ride on the earlier answer.
			if !trusted && opts.Trust != nil {
				trusted, err = opts.Trust.approved(project)
				errs = append(errs, err)
			}
			servers, err := parseConfig(project.data, project.path, OriginProject, "")
			errs = append(errs, err)
			found = append(found, servers...)
		}
	}
	return admit(found, trusted), errors.Join(errs...)
}

// admit sorts found into the servers that may start and those refused.
func admit(found []Server, trustProject bool) Config {
	var cfg Config
	taken := map[string]Origin{}
	for _, s := range found {
		if err := refusal(s, taken, trustProject); err != nil {
			cfg.Refused = append(cfg.Refused, Refusal{Server: s, Err: err})
			continue
		}
		taken[s.Name] = s.Origin
		cfg.Servers = append(cfg.Servers, s)
	}
	slices.SortFunc(cfg.Servers, func(a, b Server) int { return strings.Compare(a.Name, b.Name) })
	return cfg
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

// parseConfig parses a config file's bytes; path labels the result.
func parseConfig(data []byte, path string, origin Origin, prefix string) ([]Server, error) {
	var file struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", path, err)
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
		// The list has no separate arguments, so here spaces split them.
		fields := strings.Fields(command)
		servers = append(servers, Server{
			Name: strings.TrimSpace(name), Command: fields[0], Args: fields[1:],
			Origin: OriginUser, Source: source,
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
