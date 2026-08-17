#!/bin/bash
set -e

MODULES="ai agent engine kit coding tui cmd/nib-code cmd/agent cmd/nibster"

# Formatting is checked, not rewritten: a hook that silently reformats
# leaves the commit and the working tree out of sync.
echo "Checking formatting..."
unformatted=""
for mod in $MODULES; do
  files=$(cd "$mod" && gofmt -l .)
  if [ -n "$files" ]; then
    unformatted="$unformatted$(echo "$files" | sed "s|^|$mod/|")\n"
  fi
done
if [ -n "$unformatted" ]; then
  echo "Error: files not gofmt-formatted (run 'make fmt'):"
  printf '%b' "$unformatted"
  exit 1
fi

echo "Vetting..."
make vet

echo "Linting..."
make lint

echo "✓ All checks passed"
