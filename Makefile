BIN_DIR := $(HOME)/.local/bin

# Every Go module in the repo (each has its own go.mod). Library modules
# first, binaries last. Per-module targets below loop over this list.
MODULES := ai agent engine kit coding tui cmd/nib-code cmd/agent cmd/nib

# Modules published for external consumers, in dependency order.
# release.yml tags exactly these, in lockstep.
RELEASED_MODULES := ai agent kit

# Tree-sitter grammars whose highlights.scm we vendor into engine/highlight/queries/<lang>/.
# Format: <lang>:<module-path>. Add a new language by appending one line.
QUERY_LANGS := \
	go:github.com/tree-sitter/tree-sitter-go \
	lua:github.com/tree-sitter-grammars/tree-sitter-lua \
	yaml:github.com/tree-sitter-grammars/tree-sitter-yaml

.PHONY: all build clean install uninstall test fmt fmt-check mod-tidy-check vet lint consumer-smoke api-check sync-queries run-nib

all: build

# Compile every module, then link the three binaries.
build:
	@set -e; for mod in $(MODULES); do echo "build $$mod"; (cd $$mod && go build ./...); done
	mkdir -p cmd/nib-code/bin cmd/agent/bin cmd/nib/bin
	cd cmd/nib-code && go build -o bin/nib-code .
	cd cmd/agent && go build -o bin/nib-agent .
	cd cmd/nib && go build -o bin/nib .

install: build
	mkdir -p $(BIN_DIR)
	ln -sf $(CURDIR)/cmd/nib-code/bin/nib-code $(BIN_DIR)/nib-code
	ln -sf $(CURDIR)/cmd/agent/bin/nib-agent $(BIN_DIR)/nib-agent
	ln -sf $(CURDIR)/cmd/nib/bin/nib $(BIN_DIR)/nib
	@echo "Installed. Ensure $(BIN_DIR) is in PATH."

uninstall:
	rm -f $(BIN_DIR)/nib-code $(BIN_DIR)/nib-agent $(BIN_DIR)/nib

test:
	@set -e; for mod in $(MODULES); do echo "test $$mod"; (cd $$mod && go test -race ./...); done

fmt:
	@set -e; for mod in $(MODULES); do (cd $$mod && go fmt ./...); done

fmt-check:
	@set -e; failed=0; \
	for mod in $(MODULES); do \
		files=$$(cd $$mod && gofmt -l .); \
		if [ -n "$$files" ]; then printf 'unformatted files in %s:\n%s\n' "$$mod" "$$files"; failed=1; fi; \
	done; \
	if [ $$failed -ne 0 ]; then echo "Error: run 'make fmt'" >&2; exit 1; fi

mod-tidy-check:
	@set -e; for mod in $(MODULES); do echo "tidy $$mod"; (cd $$mod && go mod tidy -diff); done

vet:
	@set -e; for mod in $(MODULES); do echo "vet $$mod"; (cd $$mod && go vet ./...); done

# golangci-lint per module (each module carries its own .golangci.yml
# depguard rules). Requires golangci-lint on PATH.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "Error: golangci-lint is not installed. See https://golangci-lint.run/welcome/install/" >&2; exit 1; }
	@set -e; for mod in $(MODULES); do echo "lint $$mod"; (cd $$mod && golangci-lint run ./...); done

# Build the released modules as an external consumer resolves them (no
# replace). Needs network; run by CI, not by pre-commit.sh.
consumer-smoke:
	@bash consumer-smoke.sh $(RELEASED_MODULES)

# Compare the stable surface (STABILITY.md) with the last release. Needs
# apidiff on PATH and release tags; run by CI, not by pre-commit.sh.
api-check:
	@bash api-check.sh $(RELEASED_MODULES)

clean:
	rm -f cmd/nib-code/bin/*
	rm -f cmd/agent/bin/*
	rm -f cmd/nib/bin/*

# Run nib from source. Pass extra flags via ARGS, e.g.:
#   make run-nib ARGS="-m 'list files' -debug"
run-nib:
	cd cmd/nib && go run . $(ARGS)

# Re-vendor highlights.scm for every language in QUERY_LANGS, resolving the
# exact on-disk path from each module's current pinned version. Run after
# bumping a tree-sitter grammar dependency.
sync-queries:
	@set -e; for pair in $(QUERY_LANGS); do \
		lang=$${pair%%:*}; \
		mod=$${pair#*:}; \
		dir=$$(cd engine && go list -m -f '{{.Dir}}' $$mod); \
		src=$$dir/queries/highlights.scm; \
		dst=engine/highlight/queries/$$lang/highlights.scm; \
		if [ ! -f "$$src" ]; then echo "missing: $$src" >&2; exit 1; fi; \
		mkdir -p "engine/highlight/queries/$$lang"; \
		install -m 644 "$$src" "$$dst"; \
		echo "synced $$mod@$$(cd engine && go list -m -f '{{.Version}}' $$mod) -> $$dst"; \
	done
