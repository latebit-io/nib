package files

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/latebit-io/nib/kit/internal/filecap"
	"github.com/latebit-io/nib/kit/tools/search"
)

// maxSearchFileBytes is the largest file a search reads. Bigger ones
// are generated or data, and would dominate the time.
const maxSearchFileBytes = 1 << 20

// maxMatchText is how much of a matching line a result carries.
const maxMatchText = 300

// Search finds pattern in the text files under the working directory.
// It is a [search.SearchFunc]: paths in the results are relative to the
// working directory, as the other tools read them. The root argument is unused.
func (t *Tools) Search(ctx context.Context, _, pattern string, opts search.Options) ([]search.Result, error) {
	expr := pattern
	if !opts.Regex {
		expr = regexp.QuoteMeta(pattern)
	}
	if !opts.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", pattern, err)
	}
	if _, err := path.Match(opts.FileGlob, ""); err != nil {
		return nil, fmt.Errorf("file_glob %q: %w", opts.FileGlob, err)
	}
	limit := opts.MaxResults
	if limit <= 0 {
		limit = search.DefaultMaxResults
	}
	var results []search.Result
	walked, exhausted := 0, false
	err = fs.WalkDir(t.root.FS(), filepath.ToSlash(t.base), func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped; the rest is still searched.
			return fs.SkipDir
		}
		if len(results) >= limit || ctx.Err() != nil {
			return fs.SkipAll
		}
		if walked++; walked > maxWalked {
			exhausted = true
			return fs.SkipAll
		}
		if d.IsDir() {
			// Neither holds project files, and either can use up the walk.
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if ok, _ := path.Match(opts.FileGlob, d.Name()); opts.FileGlob != "" && !ok {
			return nil
		}
		shown, err := filepath.Rel(t.base, filepath.FromSlash(name))
		if err != nil {
			return nil
		}
		results = t.grep(results, re, filepath.FromSlash(name), filepath.ToSlash(shown), limit)
		return nil
	})
	switch {
	case err != nil:
		return nil, err
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case exhausted:
		// Not the same as "no match": the rest of the tree was not read.
		return results, fmt.Errorf("only the first %d entries were searched: %w", maxWalked, search.ErrIncomplete)
	}
	return results, nil
}

// grep appends the lines of one file that match, up to limit results
// in all. A file that cannot be read, is too large or is binary is skipped.
func (t *Tools) grep(results []search.Result, re *regexp.Regexp, name, shown string, limit int) []search.Result {
	data, err := filecap.ReadRoot(t.root, name, maxSearchFileBytes)
	if err != nil || bytes.IndexByte(data, 0) >= 0 {
		return results
	}
	line := 0
	for text := range strings.Lines(string(data)) {
		line++
		if !re.MatchString(text) {
			continue
		}
		text = strings.TrimSpace(text)
		if len(text) > maxMatchText {
			text = strings.ToValidUTF8(text[:maxMatchText], "") + "…"
		}
		results = append(results, search.Result{Path: shown, Line: line, Text: text})
		if len(results) >= limit {
			break
		}
	}
	return results
}
