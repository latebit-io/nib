package pluginstore

import (
	"fmt"
	"strings"
)

// SourceType enumerates the ways a plugin (or marketplace) can be
// located. It mirrors the source kinds Claude Code marketplaces use so
// an imported marketplace.json maps onto nib's model without loss.
type SourceType string

const (
	// SourceLocal is a path on the local filesystem. Copied verbatim;
	// the resolved pin is empty (local sources are not version-tracked).
	SourceLocal SourceType = "local"
	// SourceGit is a clone-able git repository (any transport git
	// understands). Ref selects a branch/tag/commit; the resolved pin is
	// the checked-out commit SHA.
	SourceGit SourceType = "git"
	// SourceGitHub is shorthand for a github.com repository named
	// "owner/repo". Expanded to an HTTPS git URL at fetch time.
	SourceGitHub SourceType = "github"
	// SourceNPM is an npm package. Not implemented in M0 — recorded so a
	// marketplace listing one round-trips, and resolved in M5.
	SourceNPM SourceType = "npm"
)

// Source locates a plugin or marketplace. Only the fields relevant to
// Type are populated; the rest stay zero. It is a value type and JSON
// round-trips for the registry ledger.
type Source struct {
	// Type selects which of the remaining fields are meaningful.
	Type SourceType `json:"type"`
	// Path is the filesystem path for [SourceLocal].
	Path string `json:"path,omitempty"`
	// Repo is the "owner/repo" identifier for [SourceGitHub].
	Repo string `json:"repo,omitempty"`
	// URL is the clone URL for [SourceGit].
	URL string `json:"url,omitempty"`
	// Subdir restricts the plugin to a subdirectory within a git
	// repository (CC's git-subdir source). Empty means the repo root.
	Subdir string `json:"subdir,omitempty"`
	// Ref is the branch, tag, or commit to check out for git sources.
	// Empty means the remote's default branch.
	Ref string `json:"ref,omitempty"`
	// Package is the npm package name for [SourceNPM].
	Package string `json:"package,omitempty"`
	// Registry overrides the default npm registry for [SourceNPM].
	Registry string `json:"registry,omitempty"`
}

// LocalSource returns a [SourceLocal] for path.
func LocalSource(path string) Source { return Source{Type: SourceLocal, Path: path} }

// GitSource returns a [SourceGit] for url at the given ref (ref may be
// empty for the default branch).
func GitSource(url, ref string) Source { return Source{Type: SourceGit, URL: url, Ref: ref} }

// GitHubSource returns a [SourceGitHub] for "owner/repo" at ref.
func GitHubSource(repo, ref string) Source {
	return Source{Type: SourceGitHub, Repo: repo, Ref: ref}
}

// Validate reports whether the Source is internally consistent — the
// fields its Type requires are present. It does not touch the network or
// filesystem.
func (s Source) Validate() error {
	switch s.Type {
	case SourceLocal:
		if s.Path == "" {
			return fmt.Errorf("pluginstore: local source missing path")
		}
	case SourceGit:
		if s.URL == "" {
			return fmt.Errorf("pluginstore: git source missing url")
		}
	case SourceGitHub:
		if s.Repo == "" {
			return fmt.Errorf("pluginstore: github source missing repo")
		}
		// Require exactly two non-empty segments: "owner/repo". A bare
		// Contains("/") would accept "/repo", "owner/", and
		// "owner/repo/extra", which only fail later at clone time.
		parts := strings.Split(s.Repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("pluginstore: github source repo %q must be owner/repo", s.Repo)
		}
	case SourceNPM:
		if s.Package == "" {
			return fmt.Errorf("pluginstore: npm source missing package")
		}
	case "":
		return fmt.Errorf("pluginstore: source missing type")
	default:
		return fmt.Errorf("pluginstore: unknown source type %q", s.Type)
	}
	return nil
}

// gitURL returns the clone URL for git-flavoured sources, expanding the
// github "owner/repo" shorthand. Returns an error for non-git types.
func (s Source) gitURL() (string, error) {
	switch s.Type {
	case SourceGit:
		return s.URL, nil
	case SourceGitHub:
		return "https://github.com/" + s.Repo + ".git", nil
	default:
		return "", fmt.Errorf("pluginstore: source type %q is not a git source", s.Type)
	}
}

// String renders a short human-readable description for logs and the
// `/plugin` listing.
func (s Source) String() string {
	switch s.Type {
	case SourceLocal:
		return "local:" + s.Path
	case SourceGit:
		return gitRefString("git:"+s.URL, s.Ref)
	case SourceGitHub:
		return gitRefString("github:"+s.Repo, s.Ref)
	case SourceNPM:
		if s.Registry != "" {
			return fmt.Sprintf("npm:%s@%s (%s)", s.Package, s.Ref, s.Registry)
		}
		return "npm:" + s.Package
	default:
		return string(s.Type)
	}
}

func gitRefString(base, ref string) string {
	if ref == "" {
		return base
	}
	return base + "@" + ref
}
