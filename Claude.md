# CLAUDE.md

## Craft Standards

- **No sycophancy — code or ideas.** Critically review code before presenting: layering violations, missing edge cases, state desync, stale references, channel blocking, rune vs byte vs cell-width confusion, silent error paths, leaky abstractions, wrong architectural layer. Challenge ideas and proposals before agreeing: what's the downside? What breaks? What's the simpler alternative? Is this solving the right problem? Push back when something doesn't hold up — disagreement backed by reasoning is expected.
- **Never silently swallow errors.** Every error must be explicitly handled — logged or surfaced. No `_ = fn()` without a comment.
- **Never hand-roll stdlib functions.** Use `slices.Sort`, `maps.Copy`, `strings.Cut`, etc. — never reimplement standard library functionality to "avoid an import."
- **After each implementation, check for SOLID/hexagonal violations, verify no tui↔engine abstraction leaks, and ensure doc comments on all exported symbols.**
- **If pre-existing issues are found, fix, always leave the code in better state for the next feature**
- **After each implementation run tests and pre-commit.sh.**

## demarkus-soul

All project context — architecture, patterns, build commands, conventions, debugging notes, and roadmap — lives on the demarkus-soul MCP server.

**important**: never post sensitive information such as api keys, passwords, and anything that can be doc'd 

### Required Preflight (Every Session)

1. `mark_fetch` `/index.md` — get the hub page
2. `mark_fetch` `/patterns.md` — build commands, code style, workflow
3. Fetch other pages as needed: `/architecture.md`, `/debugging.md`, `/roadmap.md`
4. If MCP is unavailable, stop and ask the developer before proceeding

### During Work

- Update soul pages when learning something new
- Use `mark_append` for journal entries and incremental notes
- Always use `expected_version` from a prior fetch when publishing or appending
- After each implementation run tests and pre-commit.sh

### End of Session

- Add a journal entry to `/journal.md` if something significant happened

### Content Structure

```
/index.md          — Hub page, add project workspace here 
/nib/index.md    - Project specific workspace
/nib/architecture.md   — System design, module boundaries, key decisions
/nib/patterns.md       — Code patterns, build commands, conventions, workflow
/nib/debugging.md      — Lessons from bugs and investigations
/nib/roadmap.md        — What's done, what's next
/nib/journal.md        — Session notes and evolution log
/nib/thoughts.md       — Agent reflections and ideas
/nib/guide.md          — Setup instructions for demarkus-soul
```
### Plans
 - Agents should never store the plans in their own vendor specific folder
 - The agent should always publish the plan to demarkus soul instead
