PLUGIN_DIR := $(HOME)/.config/micro/plug/agent
BIN_DIR := $(HOME)/.local/bin

.PHONY: build clean install uninstall test

build:
	cd protocol && go build ./...
	cd server && go build -o bin/junto-server ./cmd/junto-server
	cd bridge && go build -o bin/junto-bridge ./cmd/junto-bridge

install: build
	@# Install plugin
	mkdir -p $(PLUGIN_DIR)
	ln -sf $(CURDIR)/plugin/main.lua $(PLUGIN_DIR)/main.lua
	ln -sf $(CURDIR)/plugin/json.lua $(PLUGIN_DIR)/json.lua
	ln -sf $(CURDIR)/plugin/repo.json $(PLUGIN_DIR)/repo.json
	@# Install binaries
	mkdir -p $(BIN_DIR)
	ln -sf $(CURDIR)/server/bin/junto-server $(BIN_DIR)/junto-server
	ln -sf $(CURDIR)/bridge/bin/junto-bridge $(BIN_DIR)/junto-bridge
	@echo "Installed. Ensure $(BIN_DIR) is in PATH."
	@echo "Start server: junto-server"
	@echo "In micro: :agent-start <socket-path>"

uninstall:
	rm -f $(BIN_DIR)/junto-server $(BIN_DIR)/junto-bridge
	rm -rf $(PLUGIN_DIR)

test:
	cd protocol && go test ./...
	cd server && go test ./...
	cd bridge && go test ./...

clean:
	rm -f server/bin/* bridge/bin/*
