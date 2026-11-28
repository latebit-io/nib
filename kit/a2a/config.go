// Package a2a connects nib to other agents over Agent2Agent (A2A v1.0):
// each remote agent the person names becomes a tool, and [NewServer]
// serves a session as one. It wraps the official Go SDK, kept inside.
package a2a

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/internal/filecap"
	"github.com/latebit-io/nib/kit/internal/httporigin"
	"github.com/latebit-io/nib/kit/internal/userconfig"
)

// EnvKey names the environment variable that holds the person's agent
// list, in the format of the config file.
const EnvKey = brand.EnvPrefix + "A2A"

// ToolNamePrefix starts the name of every remote agent's tool.
const ToolNamePrefix = "a2a__"

// maxConfigBytes caps the config file. Real ones are a few hundred bytes.
const maxConfigBytes = 256 << 10

// maxToolName is the longest tool name the providers accept.
const maxToolName = 64

// Agent is one remote agent the person named.
type Agent struct {
	// Name is the person's name for it; its tool is ToolNamePrefix+Name.
	Name string
	// URL is the agent's address, where its card is found, or the card's own.
	URL string
	// Headers are sent with every request to the agent's origin, and only there.
	Headers map[string]string
	// Source is the file or variable the definition came from.
	Source string
}

// Refusal is an agent left out, and why.
type Refusal struct {
	Agent Agent
	Err   error
}

// Config is the person's agent list.
type Config struct {
	// Agents may be connected to.
	Agents []Agent
	// Refused could not be used, each with its reason.
	Refused []Refusal
}

// DefaultConfigPath is the person's agent list in the user config directory.
func DefaultConfigPath() (string, error) {
	return configFile("a2a.json")
}

// configFile is name in nib's user config directory.
func configFile(name string) (string, error) {
	path, err := userconfig.Path(name)
	if err != nil {
		return "", fmt.Errorf("a2a: %w", err)
	}
	return path, nil
}

// LoadConfig reads the agent list at path, where a missing file is an
// empty list, then env (the [EnvKey] value). A name both define is the
// environment's. A list that cannot be read is in err; the other still loads.
func LoadConfig(path, env string) (Config, error) {
	byName := map[string]Agent{}
	add := func(l list, source string) {
		for name, def := range l.Agents {
			byName[name] = Agent{Name: name, URL: def.URL, Headers: def.Headers, Source: source}
		}
	}
	// A list that failed to decode may be half filled in: none of it is used.
	var errs []error
	if path != "" {
		var file list
		if _, err := filecap.ReadJSON(path, maxConfigBytes, "A2A config", &file); err != nil {
			errs = append(errs, fmt.Errorf("a2a: %w", err))
		} else {
			add(file, path)
		}
	}
	if env != "" {
		var fromEnv list
		if err := json.Unmarshal([]byte(env), &fromEnv); err != nil {
			errs = append(errs, fmt.Errorf("a2a: %s: %w", EnvKey, err))
		} else {
			add(fromEnv, EnvKey)
		}
	}
	var cfg Config
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		a := byName[name]
		var err error
		if a.Headers, err = httporigin.ExpandHeaders(a.Headers); err == nil {
			err = check(a)
		}
		if err != nil {
			cfg.Refused = append(cfg.Refused, Refusal{Agent: a, Err: err})
			continue
		}
		cfg.Agents = append(cfg.Agents, a)
	}
	return cfg, errors.Join(errs...)
}

// list is the shape of the config file and of the [EnvKey] value.
type list struct {
	Agents map[string]struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"agents"`
}

// validName is what a tool name may hold after its prefix.
var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// check returns why a may not be used, or nil when it may.
func check(a Agent) error {
	switch {
	case !validName.MatchString(a.Name):
		return errors.New("a name holds only letters, digits, _ and -")
	case len(ToolNamePrefix+a.Name) > maxToolName:
		return fmt.Errorf("the name is longer than %d characters", maxToolName-len(ToolNamePrefix))
	case a.URL == "":
		return errors.New("no url")
	}
	return httporigin.CheckEndpoint(a.URL)
}
