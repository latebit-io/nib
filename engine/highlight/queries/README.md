# Vendored tree-sitter highlight queries

Files under `<lang>/highlights.scm` are **verbatim copies** of the corresponding
`queries/highlights.scm` from each tree-sitter grammar package we depend on.

Do not hand-edit these files. Local changes are overwritten on the next
`make sync-queries` run, which re-copies from the pinned module version in
`$GOMODCACHE`.

If something in a vendored query looks wrong (e.g. a duplicate capture, a
typo, a missing pattern), the fix belongs upstream — file a PR against the
grammar repo, bump the dependency here, then `make sync-queries`.

Language → upstream mapping lives in the `QUERY_LANGS` list at the top of
the repo-root `Makefile`.
