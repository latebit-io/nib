#!/bin/bash
set -e

echo "Running code formatting and linting..."
make fmt
make vet

if ! command -v golangci-lint &>/dev/null; then
  echo "Error: golangci-lint is not installed."
  echo "Install it: https://golangci-lint.run/welcome/install/"
  exit 1
fi

for mod in ai agent engine kit coding tui cmd/nib-code cmd/agent cmd/nibster; do
  echo "Linting ${mod}..."
  (cd "$mod" && golangci-lint run ./...)
done

echo "✓ All checks passed"
