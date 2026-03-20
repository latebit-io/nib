PLUGIN_DIR := $(HOME)/.config/micro/plug/agent
BIN_DIR := $(HOME)/.local/bin

.PHONY: build clean install uninstall test fmt vet

build:
	cd protocol && go build ./...
	mkdir -p server/bin
	cd server && go build -o bin/junto-server ./cmd/junto-server
	mkdir -p bridge/bin
	cd bridge && go build -o bin/junto-bridge ./cmd/junto-bridge
	mkdir -p tui/bin
	cd tui && go build -o bin/junto ./cmd/junto

install: build
	@# Install plugin (legacy Micro)
	mkdir -p $(PLUGIN_DIR)
	ln -sf $(CURDIR)/plugin/main.lua $(PLUGIN_DIR)/main.lua
	ln -sf $(CURDIR)/plugin/repo.json $(PLUGIN_DIR)/repo.json
	@# Install binaries
	mkdir -p $(BIN_DIR)
	ln -sf $(CURDIR)/server/bin/junto-server $(BIN_DIR)/junto-server
	ln -sf $(CURDIR)/bridge/bin/junto-bridge $(BIN_DIR)/junto-bridge
	ln -sf $(CURDIR)/tui/bin/junto $(BIN_DIR)/junto
	@echo "Installed. Ensure $(BIN_DIR) is in PATH."
	@echo "TUI: junto <file>"
	@echo "Legacy: junto-server + micro"

uninstall:
	rm -f $(BIN_DIR)/junto-server $(BIN_DIR)/junto-bridge $(BIN_DIR)/junto
	rm -rf $(PLUGIN_DIR)

test:
	cd protocol && go test ./...
	cd server && go test ./...
	cd bridge && go test ./...
	cd tui && go test ./...

fmt:
	cd protocol && go fmt ./...
	cd server && go fmt ./...
	cd bridge && go fmt ./...
	cd tui && go fmt ./...

vet:
	cd protocol && go vet ./...
	cd server && go vet ./...
	cd bridge && go vet ./...
	cd tui && go vet ./...

clean:
	rm -f server/bin/* bridge/bin/* tui/bin/*
