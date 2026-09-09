#!/bin/bash
set -e

# Formatting is checked, not rewritten: a hook that silently reformats
# leaves the commit and the working tree out of sync.
echo "Checking formatting..."
make fmt-check

echo "Checking module files..."
make mod-tidy-check

echo "Vetting..."
make vet

echo "Linting..."
make lint

echo "✓ All checks passed"
