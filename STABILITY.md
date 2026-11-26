# Stability

nib releases three modules in lockstep: `ai`, `agent`, and `kit`. They are on
v0. This file says which part of their exported API you can build on, what
happens when that part changes, and when v1 will be considered.

## Stable surface

| Module  | Covered                                |
| ------- | -------------------------------------- |
| `ai`    | every package outside `internal/`      |
| `agent` | every package outside `internal/`      |
| `kit`   | package `kit` and package `kit/event`  |

Every other `kit` package is plumbing for nib's own binaries (nib-code,
nib-agent, nibster). It is exported so those binaries can share it, and it can
change in any release.

`coding`, `tui`, `engine`, and `cmd/*` are not released and carry no promise.

## What compatible means

A release is compatible when code that compiled against the previous release
still compiles: nothing exported from the stable surface is removed, renamed,
or given a different type. Additions are always allowed.

Behaviour described in a doc comment is part of the contract too. Changing it
(event order, error identity, when a call returns) is a break.

## Breaking the stable surface

A break is allowed on v0, but never by accident:

1. Mark the pull request title with `!` before the colon, as in
   `feat(llm)!: pass ctx to OnRetry`. Any type and scope work.
2. Say in the description what a consumer must change.

A marked title releases as a minor version, and the marker stays in that
release's commit title. No change produces a major version while the modules
are on v0.

Pull requests are squash-merged with the title as the commit subject. Keep the
marker when editing the subject in the merge dialog; the release reads it from
there.

## The check

`make api-check` compares the stable surface with the last release the tree
contains, using `apidiff` from `golang.org/x/exp`; CI runs it on every pull
request and fails an unmarked break. It sees declarations only: behaviour
changes depend on review and on the tests of the code that consumes nib.

## Toward v1

v1.0.0 will be reconsidered when either holds:

- a consumer outside this project's control depends on nib, or
- three months pass without a marked break on the stable surface.

Before any v1 tag, the `kit` packages outside the stable surface have to be
trimmed, moved, or released separately. Go's versioning covers a whole module,
so a v1 `kit` would promise all of them.
