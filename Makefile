BIN_DIR := $(HOME)/.local/bin

.PHONY: build clean install uninstall test fmt vet

build:
	cd engine && go build ./...
	mkdir -p tui/bin
	cd tui && go build -o bin/junto ./cmd/junto

install: build
	mkdir -p $(BIN_DIR)
	ln -sf $(CURDIR)/tui/bin/junto $(BIN_DIR)/junto
	@echo "Installed. Ensure $(BIN_DIR) is in PATH."
	@echo "Usage: junto <file>"

uninstall:
	rm -f $(BIN_DIR)/junto

test:
	cd engine && go test ./...
	cd tui && go test ./...

fmt:
	cd engine && go fmt ./...
	cd tui && go fmt ./...

vet:
	cd engine && go vet ./...
	cd tui && go vet ./...

clean:
	rm -f tui/bin/*
