package skill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/kit/internal/filecap"
)

// maxListedResources caps the bundled-file listing; past it the note
// says the listing is incomplete.
const maxListedResources = 50

// maxWalkedEntries bounds the directory entries the listing reads, so a
// project skill with a huge tree cannot stall an activation.
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
// listing short. A directory that cannot be read does not stop the rest.
func bundled(dir string) (files []string, more bool, err error) {
	budget := maxWalkedEntries
	var errs []error
	pending := []string{"."}
	for len(pending) > 0 && budget > 0 {
		rel := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		// One past the budget tells a full directory from a cut one.
		entries, readErr := readEntries(filepath.Join(dir, rel), budget+1)
		if readErr != nil {
			errs = append(errs, readErr)
		}
		if len(entries) > budget {
			entries, more = entries[:budget], true
		}
		budget -= len(entries)
		for _, e := range entries {
			name := filepath.Join(rel, e.Name())
			switch t := e.Type(); {
			case t.IsDir():
				// Dot directories (.git) are tooling state, not skill content.
				if !strings.HasPrefix(e.Name(), ".") {
					pending = append(pending, name)
				}
			case name == skillFile:
			case t.IsRegular() || t&fs.ModeSymlink != 0:
				// Pipes, sockets and devices are not readable resources.
				files = append(files, filepath.ToSlash(name))
			}
		}
	}
	if len(pending) > 0 {
		more = true
	}
	slices.Sort(files)
	if len(files) > maxListedResources {
		files, more = files[:maxListedResources], true
	}
	return files, more, errors.Join(errs...)
}

// readEntries returns at most n entries of dir, in directory order.
// WalkDir would read and sort the whole directory before any cap applies.
func readEntries(dir string, n int) ([]fs.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only handle; nothing to flush
	entries, err := f.ReadDir(n)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return entries, err
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
	data, err := filecap.ReadRoot(root, filepath.FromSlash(name), maxResourceBytes)
	if errors.Is(err, filecap.ErrTooLarge) {
		return "", fmt.Errorf("%w; process it with a script instead", err)
	}
	if err != nil {
		return "", err
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
