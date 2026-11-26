package skill

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
)

// maxListedResources caps the bundled-file listing; past it the note
// says the listing is incomplete.
const maxListedResources = 50

// maxWalkedEntries bounds the listing walk itself, so a project skill
// with a huge tree and few files cannot stall an activation.
const maxWalkedEntries = 1000

// maxResourceBytes caps one bundled file read through a skill tool.
// Larger files are for the skill's scripts, not the context window.
const maxResourceBytes = 256 << 10

// ResourceToolName is the tool [ResourceTool] returns. It lacks
// [ToolNamePrefix] on purpose: it reads files and is not a skill.
const ResourceToolName = "read_skill_file"

// realDir returns path made absolute with its symlinks resolved.
func realDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// ResourceNote returns the text to append to the skill's instructions
// on activation: its directory and its bundled files, listed but not
// read. readTool names the tool that reads one. Empty without files.
func (s Skill) ResourceNote(readTool string) string {
	if s.Dir == "" {
		return ""
	}
	files, incomplete, err := bundled(s.Dir)
	if err != nil {
		slog.Warn("skill: bundled-file listing is incomplete", "skill", s.Name, "dir", s.Dir, "err", err)
		incomplete = true
	}
	if len(files) == 0 && !incomplete {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nSkill directory: %s\n", s.Dir)
	b.WriteString("Relative paths in this skill are relative to the skill directory.\n")
	fmt.Fprintf(&b, "To read a bundled file, call %s with the \"file\" argument.\n", readTool)
	b.WriteString("<skill_resources>\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  <file>%s</file>\n", f)
	}
	if incomplete {
		b.WriteString("  (listing incomplete: the directory holds more files)\n")
	}
	b.WriteString("</skill_resources>")
	return b.String()
}

// bundled lists the files under dir other than SKILL.md, slash-separated
// and relative to dir, in lexical order. more reports that a cap cut the
// listing short.
func bundled(dir string) (files []string, more bool, err error) {
	walked := 0
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if walked++; walked > maxWalkedEntries {
			more = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			// Dot directories (.git) are tooling state, not skill content.
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if rel == skillFile {
			return nil
		}
		if len(files) == maxListedResources {
			more = true
			return filepath.SkipAll
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, more, err
}

// ReadResource returns a file bundled with the skill. name is relative
// to the skill directory; a path that leaves it, through ".." or a
// symlink, is refused.
func (s Skill) ReadResource(name string) (string, error) {
	if s.Dir == "" {
		return "", fmt.Errorf("skill %q has no directory", s.Name)
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }() // read-only handle; nothing to flush
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }() // read-only handle; nothing to flush
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}
	// Read one byte past the cap: the size from Stat can be stale.
	data, err := io.ReadAll(io.LimitReader(f, maxResourceBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxResourceBytes {
		return "", fmt.Errorf("%s is over the %d-byte read cap; process it with a script instead", name, maxResourceBytes)
	}
	return string(data), nil
}

// resourceArgs is the argument object for reading a bundled file.
type resourceArgs struct {
	File string `json:"file" desc:"Bundled file to read, relative to the skill directory."`
}

// ResourceTool returns a tool named [ResourceToolName] that only reads
// the skill's bundled files. A fork skill's child agent gets it: the
// child cannot call the tool that spawned it.
func ResourceTool(s Skill) upagent.Tool {
	return upagent.NewTool(ResourceToolName, "Read a file bundled with the "+s.Name+" skill.",
		func(_ context.Context, args resourceArgs) (string, error) {
			return s.ReadResource(args.File)
		})
}
