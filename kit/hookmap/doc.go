// Package hookmap maps between nib's builtin tool names and the Claude
// Code (CC) tool names that plugin hook matchers are written against.
//
// CC hook groups carry a regex matcher tested against a tool name (e.g.
// `Write|Edit|Bash`). nib dispatches its own builtin names (write_file,
// edit_file, bash, …), so a CC-authored matcher would never match a nib
// call unless the names are reconciled. This package owns that alias
// table and the [Matches] helper the hooks dispatcher uses: it tests a
// group's matcher against BOTH the nib tool name and its CC alias, so a
// matcher written either way fires correctly.
//
// The package is pure (no I/O) and depends only on [hookspec] for the
// group type, mirroring the self-contained, table-tested shape of
// [github.com/latebit-io/nib/kit/toolperm].
package hookmap
