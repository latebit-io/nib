// Package search provides project-wide text search.
// Shells out to ripgrep (rg) for speed when available, falls back to a
// pure-Go implementation using filelist.Walk + regexp.
package search

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/latebit-io/nib/engine/filelist"
)

// maxScanToken is the maximum line length the scanner will handle (1MB).
// Prevents "token too long" errors on minified files or long JSON lines.
const maxScanToken = 1024 * 1024

// Result represents a single search match.
type Result struct {
	// Path is the file path relative to the search root.
	Path string
	// Line is the 1-indexed line number.
	Line int
	// Col is the 0-indexed byte column of the match start.
	Col int
	// Text is the content of the matching line.
	Text string
}

// Options controls search behavior.
type Options struct {
	// CaseSensitive controls case matching. Default false (case-insensitive).
	CaseSensitive bool
	// Regex treats the pattern as a regular expression. Default false (literal).
	Regex bool
	// MaxResults caps the number of results returned. 0 means default (1000).
	MaxResults int
	// FileGlob filters files by glob pattern (e.g. "*.go"). Empty means all files.
	FileGlob string
}

// DefaultMaxResults is the default cap on search results when MaxResults is 0.
const DefaultMaxResults = 200

func (o Options) maxResults() int {
	if o.MaxResults > 0 {
		return o.MaxResults
	}
	return DefaultMaxResults
}

// Search runs a text search across all files under root.
// Uses ripgrep if available, otherwise falls back to Go-native search.
func Search(root, pattern string, opts Options) ([]Result, error) {
	if pattern == "" {
		return nil, nil
	}
	if _, err := exec.LookPath("rg"); err == nil {
		return searchRipgrep(root, pattern, opts)
	}
	slog.Debug("search: rg not found, using Go fallback")
	return searchGoNative(root, pattern, opts)
}

// rgJSON is the subset of ripgrep's JSON output we need.
type rgJSON struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		LineNumber     int `json:"line_number"`
		AbsoluteOffset int `json:"absolute_offset"`
		Lines          struct {
			Text string `json:"text"`
		} `json:"lines"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

// rgArgs builds the ripgrep command-line arguments for a search.
func rgArgs(pattern string, opts Options) []string {
	args := []string{
		"--json",
		"--hidden", // include dotfiles for parity with Go fallback
		"--max-count", fmt.Sprintf("%d", opts.maxResults()),
	}
	if !opts.CaseSensitive {
		args = append(args, "-i")
	}
	if !opts.Regex {
		args = append(args, "--fixed-strings")
	}
	if opts.FileGlob != "" {
		args = append(args, "--glob", opts.FileGlob)
	}
	return append(args, "--", pattern)
}

// searchRipgrep shells out to rg --json for structured results.
func searchRipgrep(root, pattern string, opts Options) ([]Result, error) {
	cmd := exec.Command("rg", rgArgs(pattern, opts)...)
	cmd.Dir = root

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rg: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rg: start: %w", err)
	}

	var results []Result
	var stoppedEarly bool
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScanToken)
	for scanner.Scan() {
		var entry rgJSON
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			slog.Debug("search: rg json parse error", "err", err)
			continue
		}
		if entry.Type != "match" {
			continue
		}
		col := 0
		if len(entry.Data.Submatches) > 0 {
			col = entry.Data.Submatches[0].Start
		}
		results = append(results, Result{
			Path: entry.Data.Path.Text,
			Line: entry.Data.LineNumber,
			Col:  col,
			Text: strings.TrimRight(entry.Data.Lines.Text, "\n\r"),
		})
		if len(results) >= opts.maxResults() {
			stoppedEarly = true
			break
		}
	}

	// Kill rg if we stopped early (maxResults reached before EOF).
	// Then wait to reap the process.
	if stoppedEarly {
		_ = cmd.Process.Kill() // intentional kill — exit error is expected
	}
	waitErr := cmd.Wait()

	// rg exits 1 when no matches — that's not an error.
	if waitErr != nil {
		if stoppedEarly {
			return results, nil
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 {
			return results, nil
		}
		return nil, fmt.Errorf("rg: %w", waitErr)
	}
	if err := scanner.Err(); err != nil {
		return results, fmt.Errorf("scan rg output: %w", err)
	}
	return results, nil
}

// compilePattern builds a regexp from the search pattern and options.
func compilePattern(pattern string, opts Options) (*regexp.Regexp, error) {
	if opts.Regex {
		p := pattern
		if !opts.CaseSensitive {
			p = "(?i)" + p
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
		return re, nil
	}
	escaped := regexp.QuoteMeta(pattern)
	if !opts.CaseSensitive {
		escaped = "(?i)" + escaped
	}
	return regexp.MustCompile(escaped), nil
}

// matchesGlob reports whether relPath matches the file glob filter.
// Uses basename for simple patterns ("*.go") and full path for
// directory patterns ("dir/*.go"), matching ripgrep --glob semantics.
func matchesGlob(glob, relPath string) (bool, error) {
	target := filepath.Base(relPath)
	if strings.Contains(glob, "/") {
		target = relPath
	}
	return filepath.Match(glob, target)
}

// searchFile scans a single file for regex matches, appending to results.
// Returns true if the max result cap was reached.
func searchFile(root, relPath string, re *regexp.Regexp, max int, results *[]Result) bool {
	absPath := filepath.Join(root, relPath)
	f, err := os.Open(absPath)
	if err != nil {
		slog.Debug("search: skip file", "path", relPath, "err", err)
		return false
	}
	defer func() { _ = f.Close() }() // best-effort: file was only opened for reading

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScanToken)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		loc := re.FindStringIndex(line)
		if loc == nil {
			continue
		}
		*results = append(*results, Result{
			Path: relPath,
			Line: lineNum,
			Col:  loc[0],
			Text: line,
		})
		if len(*results) >= max {
			return true
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		slog.Debug("search: scan error", "path", relPath, "err", scanErr)
	}
	return false
}

// searchGoNative walks the project and searches each file line by line.
func searchGoNative(root, pattern string, opts Options) ([]Result, error) {
	files, err := filelist.Walk(root)
	if err != nil && !errors.Is(err, filelist.ErrCapped) {
		return nil, fmt.Errorf("walk: %w", err)
	}

	re, err := compilePattern(pattern, opts)
	if err != nil {
		return nil, err
	}

	max := opts.maxResults()
	var results []Result

	for _, relPath := range files {
		if opts.FileGlob != "" {
			matched, matchErr := matchesGlob(opts.FileGlob, relPath)
			if matchErr != nil {
				return nil, fmt.Errorf("invalid file glob %q: %w", opts.FileGlob, matchErr)
			}
			if !matched {
				continue
			}
		}
		if searchFile(root, relPath, re, max, &results) {
			break
		}
	}
	return results, nil
}
