BIN_DIR := $(HOME)/.local/bin

# Tree-sitter grammars whose highlights.scm we vendor into engine/highlight/queries/<lang>/.
# Format: <lang>:<module-path>. Add a new language by appending one line.
QUERY_LANGS := \
	go:github.com/tree-sitter/tree-sitter-go \
	lua:github.com/tree-sitter-grammars/tree-sitter-lua

.PHONY: build clean install uninstall test fmt vet sync-queries

build:
	cd engine && go build ./...
	mkdir -p tui/bin
	cd tui && go build -o bin/junto ./cmd/junto
	mkdir -p cmd/junto-agent/bin
	cd cmd/junto-agent && go build -o bin/junto-agent .

install: build
	mkdir -p $(BIN_DIR)
	ln -sf $(CURDIR)/tui/bin/junto $(BIN_DIR)/junto
	ln -sf $(CURDIR)/cmd/junto-agent/bin/junto-agent $(BIN_DIR)/junto-agent
	@echo "Installed. Ensure $(BIN_DIR) is in PATH."

uninstall:
	rm -f $(BIN_DIR)/junto $(BIN_DIR)/junto-agent

test:
	cd engine && go test ./...
	cd tui && go test ./...
	cd cmd/junto-agent && go test ./...

fmt:
	cd engine && go fmt ./...
	cd tui && go fmt ./...
	cd cmd/junto-agent && go fmt ./...

vet:
	cd engine && go vet ./...
	cd tui && go vet ./...
	cd cmd/junto-agent && go vet ./...

clean:
	rm -f tui/bin/*
	rm -f cmd/junto-agent/bin/*

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
