// Package files provides the file tools (read, write, edit, glob,
// list) confined to one directory tree. Confinement is [os.Root]'s:
// no path or symlink reaches outside the root.
package files

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/kit/internal/filecap"
)

// maxReadBytes caps one file read into the conversation.
const maxReadBytes = 256 << 10

// maxReplyBytes caps the text one read returns. A whole file at the
// read cap would cost tens of thousands of tokens on every later turn.
const maxReplyBytes = 64 << 10

// maxWriteBytes caps the size of a file a tool call may produce.
const maxWriteBytes = 1 << 20

// maxMatches caps a glob or directory listing.
const maxMatches = 500

// maxWalked bounds the entries one glob visits, so a huge tree cannot
// stall a call.
const maxWalked = 50_000

// Tools is the file tools over one root. Close it when the session ends.
type Tools struct {
	root *os.Root
	// base is the working directory relative to the root; relative
	// paths in tool calls resolve against it.
	base string
}

// New returns file tools confined to root. Relative paths in calls
// resolve against dir, the working directory, which must be root or
// inside it.
func New(root, dir string) (*Tools, error) {
	base, err := filepath.Rel(root, dir)
	if err != nil || !filepath.IsLocal(base) {
		return nil, fmt.Errorf("files: working directory %s is not inside %s", dir, root)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	return &Tools{root: r, base: base}, nil
}

// Close releases the root directory handle.
func (t *Tools) Close() error { return t.root.Close() }

// Tools returns the five tools, in a stable order.
func (t *Tools) Tools() []agent.Tool {
	return []agent.Tool{
		agent.NewTool("read_file", "Read a text file. Returns its lines prefixed with line numbers. "+
			"Use offset and limit for a large file.", t.read),
		agent.NewTool("write_file", "Create a file or replace its whole content, creating parent directories. "+
			"To change part of an existing file, use edit_file.", t.write),
		agent.NewTool("edit_file", "Replace an exact string in a file. old_string must match the file exactly, "+
			"whitespace included, and occur once unless replace_all is set. Read the file first.", t.edit),
		agent.NewTool("glob", "Find files by pattern, such as **/*.go or cmd/*/main.go. ** matches any number "+
			"of directories. Returns paths relative to the working directory. .git and node_modules are not searched.", t.glob),
		agent.NewTool("list_dir", "List a directory's entries. Directories end with a slash.", t.list),
	}
}

// rel turns a path from a tool call into one relative to the root.
// An absolute path must lie inside the root.
func (t *Tools) rel(name string) (string, error) {
	if name == "" {
		return "", errors.New("path is empty")
	}
	p := filepath.FromSlash(name)
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(t.root.Name(), p)
		if err != nil {
			return "", fmt.Errorf("%s is outside the project", name)
		}
		p = r
	} else {
		p = filepath.Join(t.base, p)
	}
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("%s is outside the project", name)
	}
	return p, nil
}

type readArgs struct {
	Path   string `json:"path" desc:"File to read, relative to the working directory or absolute inside the project."`
	Offset int    `json:"offset,omitempty" desc:"First line to return, starting at 1. Default 1." min:"1"`
	Limit  int    `json:"limit,omitempty" desc:"Number of lines to return. Default 2000." min:"1"`
}

// defaultReadLines is how many lines a read returns when no limit is given.
const defaultReadLines = 2000

func (t *Tools) read(_ context.Context, args readArgs) (string, error) {
	data, err := t.readFile(args.Path)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "The file is empty.", nil
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	first := max(args.Offset, 1)
	if first > len(lines) {
		return "", fmt.Errorf("%s has %d lines; offset %d is past the end", args.Path, len(lines), first)
	}
	limit := args.Limit
	if limit == 0 {
		limit = defaultReadLines
	}
	last := min(first-1+limit, len(lines))
	var b strings.Builder
	for i := first - 1; i < last; i++ {
		// Always one line, then stop at the cap: the trailer says where to go on.
		if i >= first && b.Len()+len(lines[i]) > maxReplyBytes {
			last = i
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if last < len(lines) {
		fmt.Fprintf(&b, "[%d more lines; continue with offset %d]\n", len(lines)-last, last+1)
	}
	return b.String(), nil
}

// readFile returns a regular text file's bytes, refusing one over the
// read cap or with NUL bytes, which would be noise in a conversation.
func (t *Tools) readFile(name string) ([]byte, error) {
	p, err := t.rel(name)
	if err != nil {
		return nil, err
	}
	data, err := filecap.ReadRoot(t.root, p, maxReadBytes)
	if errors.Is(err, filecap.ErrTooLarge) {
		return nil, fmt.Errorf("%w; search it or read it with a shell command", err)
	}
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%s is a binary file", name)
	}
	return data, nil
}

type writeArgs struct {
	Path    string `json:"path" desc:"File to write, relative to the working directory or absolute inside the project."`
	Content string `json:"content" desc:"The complete new content of the file."`
}

func (t *Tools) write(_ context.Context, args writeArgs) (string, error) {
	if err := t.writeFile(args.Path, args.Content); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s.", len(args.Content), args.Path), nil
}

// writeFile replaces name's content, creating its parent directories.
// The content is written beside the target and renamed over it, so a
// failed write leaves the old file whole. Only regular files are replaced.
func (t *Tools) writeFile(name, content string) error {
	if len(content) > maxWriteBytes {
		return fmt.Errorf("content is over the %d-byte write cap", maxWriteBytes)
	}
	p, err := t.rel(name)
	if err != nil {
		return err
	}
	p, info, err := t.target(p)
	if err != nil {
		return err
	}
	if info != nil && !info.Mode().IsRegular() {
		// Opening a pipe to write blocks until something reads it.
		return fmt.Errorf("%s is not a regular file", name)
	}
	dir := filepath.Dir(p)
	if err := t.root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(p)+".nib-"+rand.Text()[:8])
	err = t.writeNew(tmp, content, info)
	if err == nil {
		err = t.root.Rename(tmp, p)
	}
	if err != nil {
		_ = t.root.Remove(tmp) // best effort; the error above is the one to report
		return err
	}
	return nil
}

// maxLinkHops bounds how many links a write follows to its target.
const maxLinkHops = 8

// target resolves p, link by link, to the file a write replaces; info
// is nil when nothing exists there yet. It walks one component at a time
// as the filesystem does: ".." after a linked directory leaves its target.
func (t *Tools) target(p string) (string, fs.FileInfo, error) {
	pending := strings.Split(filepath.ToSlash(p), "/")
	var resolved []string // free of links, so ".." can drop its last part
	missing := false      // a component did not exist; nothing after it can
	for hops := 0; len(pending) > 0; {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", nil, fmt.Errorf("%s is outside the project", p)
			}
			// The kernel cannot step back out of what is not there.
			if missing {
				return "", nil, fmt.Errorf("%s: %s does not exist", p, filepath.Join(resolved...))
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		cur := filepath.Join(filepath.Join(resolved...), part)
		info, err := t.root.Lstat(cur)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}
		if err != nil {
			missing = true
			resolved = append(resolved, part)
			continue
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			// Only a directory can have anything after it, ".." included.
			if len(pending) > 0 && !info.IsDir() {
				return "", nil, fmt.Errorf("%s is not a directory", cur)
			}
			resolved = append(resolved, part)
			continue
		}
		if hops++; hops > maxLinkHops {
			return "", nil, fmt.Errorf("%s: too many links", p)
		}
		link, err := t.root.Readlink(cur)
		if err != nil {
			return "", nil, err
		}
		if filepath.IsAbs(link) {
			return "", nil, fmt.Errorf("%s links to an absolute path", cur)
		}
		// The link's text takes the place of the component it replaces.
		pending = append(strings.Split(filepath.ToSlash(link), "/"), pending...)
	}
	if len(resolved) == 0 {
		return "", nil, fmt.Errorf("%s is the project directory", p)
	}
	final := filepath.Join(resolved...)
	info, err := t.root.Lstat(final)
	if errors.Is(err, fs.ErrNotExist) {
		return final, nil, nil
	}
	return final, info, err
}

// writeNew creates name with content. Replacing a file keeps that
// file's mode exactly; a new file gets 0644 less the process umask.
func (t *Tools) writeNew(name, content string, replaces fs.FileInfo) error {
	perm := fs.FileMode(0o644)
	if replaces != nil {
		perm = replaces.Mode().Perm()
	}
	f, err := t.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	// Chmod only to restore an existing mode: on a new file it would
	// undo the umask and could make a private file readable by others.
	if err == nil && replaces != nil {
		err = f.Chmod(perm)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

type editArgs struct {
	Path       string `json:"path" desc:"File to edit, relative to the working directory or absolute inside the project."`
	OldString  string `json:"old_string" desc:"Exact text to replace. Must differ from new_string."`
	NewString  string `json:"new_string" desc:"Text to put in its place. Empty deletes old_string."`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"Replace every occurrence instead of requiring exactly one."`
}

func (t *Tools) edit(_ context.Context, args editArgs) (string, error) {
	if args.OldString == "" {
		return "", errors.New("old_string is empty; to create a file use write_file")
	}
	if args.OldString == args.NewString {
		return "", errors.New("old_string and new_string are the same")
	}
	data, err := t.readFile(args.Path)
	if err != nil {
		return "", err
	}
	text := string(data)
	n := strings.Count(text, args.OldString)
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string was not found in %s; read the file and copy the text exactly", args.Path)
	case n > 1 && !args.ReplaceAll:
		return "", fmt.Errorf("old_string occurs %d times in %s; add surrounding lines to make it unique, or set replace_all", n, args.Path)
	}
	// Checked before building it: a short string replaced many times by
	// a long one can be far larger than anything the write cap allows.
	grown := int64(len(text)) + int64(n)*(int64(len(args.NewString))-int64(len(args.OldString)))
	if grown > maxWriteBytes {
		return "", fmt.Errorf("the edit would make %s %d bytes, over the %d-byte write cap", args.Path, grown, maxWriteBytes)
	}
	if err := t.writeFile(args.Path, strings.ReplaceAll(text, args.OldString, args.NewString)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s.", n, args.Path), nil
}

type globArgs struct {
	Pattern string `json:"pattern" desc:"Slash-separated pattern, relative to the directory searched. ** matches any number of directories."`
	Path    string `json:"path,omitempty" desc:"Directory to search. Default: the working directory."`
}

func (t *Tools) glob(ctx context.Context, args globArgs) (string, error) {
	// Adjacent ** match the same paths as one, and cost a search each.
	pattern := slices.CompactFunc(strings.Split(path.Clean(args.Pattern), "/"),
		func(a, b string) bool { return a == "**" && b == "**" })
	for _, segment := range pattern {
		if _, err := path.Match(segment, ""); err != nil {
			return "", fmt.Errorf("pattern %q: %w", args.Pattern, err)
		}
	}
	deep := slices.Contains(pattern, "**")
	start, err := t.rel(cmp.Or(args.Path, "."))
	if err != nil {
		return "", err
	}
	var matches []string
	walked, cut := 0, false
	err = fs.WalkDir(t.root.FS(), filepath.ToSlash(start), func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped; the rest still match.
			return fs.SkipDir
		}
		if walked++; walked > maxWalked || ctx.Err() != nil {
			cut = true
			return fs.SkipAll
		}
		below, err := filepath.Rel(start, filepath.FromSlash(name))
		if err != nil {
			return nil
		}
		segments := strings.Split(filepath.ToSlash(below), "/")
		if d.IsDir() {
			// Neither holds project files, and either can use up the walk.
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			// Without **, nothing below the pattern's depth can match.
			if !deep && below != "." && len(segments) >= len(pattern) {
				return fs.SkipDir
			}
			return nil
		}
		if !match(pattern, segments) {
			return nil
		}
		// Relative to the working directory, as every other tool reads paths.
		if shown, err := filepath.Rel(t.base, filepath.FromSlash(name)); err == nil {
			matches = append(matches, filepath.ToSlash(shown))
		}
		// One more than is shown is enough to say the listing was cut.
		if len(matches) > maxMatches {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 && !cut {
		return "No files match.", nil
	}
	return listing(matches, cut), nil
}

// match reports whether the path segments match the pattern segments.
// A ** segment matches zero or more whole segments. The work is bounded
// by pattern length times path length, whatever the pattern.
func match(pattern, name []string) bool {
	// reach[j] says the pattern so far matches the first j segments.
	reach := make([]bool, len(name)+1)
	reach[0] = true
	for _, segment := range pattern {
		next := make([]bool, len(name)+1)
		if segment == "**" {
			reached := false
			for j := range reach {
				reached = reached || reach[j]
				next[j] = reached
			}
		} else {
			for j, part := range name {
				if reach[j] {
					// The pattern's syntax was checked before the walk.
					next[j+1], _ = path.Match(segment, part)
				}
			}
		}
		reach = next
	}
	return reach[len(name)]
}

type listArgs struct {
	Path string `json:"path,omitempty" desc:"Directory to list. Default: the working directory."`
}

func (t *Tools) list(_ context.Context, args listArgs) (string, error) {
	p, err := t.rel(cmp.Or(args.Path, "."))
	if err != nil {
		return "", err
	}
	entries, err := fs.ReadDir(t.root.FS(), filepath.ToSlash(p))
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return "The directory is empty.", nil
	}
	return listing(names, false), nil
}

// listing renders names sorted, one per line, cut at maxMatches. cut
// says the search itself stopped early.
func listing(names []string, cut bool) string {
	slices.Sort(names)
	if len(names) > maxMatches {
		names, cut = names[:maxMatches], true
	}
	out := strings.Join(names, "\n")
	if cut {
		out += "\n[more entries exist; narrow the pattern or path]"
	}
	return out
}
