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
	"github.com/latebit-io/nib/kit/internal/httporigin"
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

// Server is one server definition: a program spoken to over stdio, or
// a Streamable HTTP endpoint.
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
	// URL is an HTTP server's endpoint. A server has a Command or a URL.
	URL string
	// Headers are sent with every request to an HTTP server.
	Headers map[string]string
	// ClientID is an OAuth client registered for the server ahead of
	// time, for [Login]. Empty means the client registers itself.
	ClientID string
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
	var refused []Refusal
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
		servers, bad, err := parseConfig(data, src.Path, OriginPlugin, src.Prefix+"__")
		errs = append(errs, err)
		found, refused = append(found, servers...), append(refused, bad...)
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
			servers, bad, err := parseConfig(project.data, project.path, OriginProject, "")
			errs = append(errs, err)
			found, refused = append(found, servers...), append(refused, bad...)
		}
	}
	return admit(found, refused, trusted), errors.Join(errs...)
}

// admit sorts found into the servers that may start and those refused,
// after the ones already refused while the files were read.
func admit(found []Server, refused []Refusal, trustProject bool) Config {
	cfg := Config{Refused: refused}
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

// Target is what the server is reached through: its endpoint, or its
// command line.
func (s Server) Target() string {
	if s.URL != "" {
		return s.URL
	}
	return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
}

// refusal returns why s may not start, or nil when it may.
func refusal(s Server, taken map[string]Origin, trustProject bool) error {
	switch {
	case s.URL != "" && s.Command != "":
		return errors.New("has both a command and a url")
	case s.URL != "":
		if err := httporigin.CheckEndpoint(s.URL); err != nil {
			return err
		}
	case strings.TrimSpace(s.Command) == "":
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

// definition is one entry of a config file, in the format other
// agents read.
type definition struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	OAuth   struct {
		ClientID string `json:"clientId"`
	} `json:"oauth"`
}

// parseConfig parses a config file's bytes; path labels the result. A
// definition that cannot be used becomes a refusal with its reason.
func parseConfig(data []byte, path string, origin Origin, prefix string) ([]Server, []Refusal, error) {
	var file struct {
		Servers map[string]definition `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("mcp: %s: %w", path, err)
	}
	var servers []Server
	var refused []Refusal
	// Sorted: which of two colliding names wins must not depend on map order.
	for _, name := range slices.Sorted(maps.Keys(file.Servers)) {
		def := file.Servers[name]
		s := Server{
			Name: prefix + name, Command: def.Command, Args: def.Args, Env: def.Env,
			URL: def.URL, ClientID: def.OAuth.ClientID, Origin: origin, Source: path,
		}
		var err error
		if s.Headers, err = expandHeaders(def.Headers, origin); err == nil {
			err = transport(def)
		}
		if err != nil {
			refused = append(refused, Refusal{Server: s, Err: err})
			continue
		}
		servers = append(servers, s)
	}
	return servers, refused, nil
}

// transport checks that the definition names a transport this client has.
func transport(def definition) error {
	switch def.Type {
	case "", "stdio", "http", "streamable-http", "streamableHttp":
		return nil
	case "sse":
		return errors.New("the HTTP+SSE transport is deprecated and not supported; use the server's Streamable HTTP endpoint")
	}
	return fmt.Errorf("unknown transport type %q", def.Type)
}

// expandHeaders fills ${NAME} references from the environment. A
// project's file may not use them: a repository could otherwise send
// any of the person's secrets to a server it names.
func expandHeaders(headers map[string]string, origin Origin) (map[string]string, error) {
	if key := httporigin.ReadsEnv(headers); key != "" && origin == OriginProject {
		return nil, fmt.Errorf("header %s reads the environment, which a project's file may not do", key)
	}
	return httporigin.ExpandHeaders(headers)
}

// ParseSpec parses the person's own list from the environment:
// "name=command args" or "name=https://endpoint", separated by
// semicolons. source labels the result; a bad entry does not stop the rest.
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
		name, command = strings.TrimSpace(name), strings.TrimSpace(command)
		if strings.HasPrefix(command, "https://") || strings.HasPrefix(command, "http://") {
			servers = append(servers, Server{Name: name, URL: command, Origin: OriginUser, Source: source})
			continue
		}
		// The list has no separate arguments, so here spaces split them.
		fields := strings.Fields(command)
		servers = append(servers, Server{
			Name: name, Command: fields[0], Args: fields[1:],
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
