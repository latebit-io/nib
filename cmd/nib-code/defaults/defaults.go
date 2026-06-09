// Package defaults bundles starter assets that nib-code materializes
// into a fresh project on first launch. Today the only asset class
// is markdown slash-command templates under [defaults/commands/];
// future additions (style profiles, prompt fragments) would live as
// sibling embed FS sub-trees.
//
// The embed-and-seed pattern (vs. ship-as-tracked-files in
// .project/commands/) keeps the defaults single-sourced — the
// seeded copy in any project's .project/commands/ is the same bytes
// the embedded FS holds, and a default added in a new nib-code
// release flows to every project on its next cold start.
package defaults

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"embed"
)

// commandsFS holds the markdown command templates bundled with the
// binary. The embed root is "commands" so [Seed] can walk it as the
// authoritative list of files to materialize into a project's
// commands directory.
//
//go:embed commands/*.md
var commandsFS embed.FS

// Seed materializes every bundled command template into dir if dir
// does not already exist. The "dir doesn't exist yet" check is the
// idempotency gate: once a project has any commands directory —
// even empty — Seed treats it as "user owns this now" and refuses to
// touch it. Users who don't want any defaults can `mkdir -p
// .project/commands` and Seed will leave it alone forever after.
//
// Returns:
//   - nil with no side effects when dir already exists.
//   - An error and a partially-seeded directory if a write failed
//     mid-stream. Partial seeds are preserved (not rolled back) so a
//     caller that wants strict atomicity can detect the error and
//     decide; the more common policy at startup is to log and
//     continue.
//
// dir must be an absolute path. Seed creates it (and any missing
// parents) when seeding is needed.
func Seed(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		// Dir exists — user owns it.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dir, err)
	}

	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	entries, err := fs.ReadDir(commandsFS, "commands")
	if err != nil {
		// Embed FS access can only fail under a misconfigured embed
		// directive; surface as a programmer error rather than
		// pretending nothing was bundled.
		return fmt.Errorf("read embedded commands: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, readErr := commandsFS.ReadFile("commands/" + e.Name())
		if readErr != nil {
			return fmt.Errorf("read embedded %s: %w", e.Name(), readErr)
		}
		out := filepath.Join(dir, e.Name())
		if writeErr := os.WriteFile(out, data, 0600); writeErr != nil {
			return fmt.Errorf("write %s: %w", out, writeErr)
		}
	}
	return nil
}

// CommandFiles returns the names of bundled command files. Used by
// startup logging and tests; not used in the seed path itself.
func CommandFiles() []string {
	entries, err := fs.ReadDir(commandsFS, "commands")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}
