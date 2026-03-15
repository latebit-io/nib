PLUGIN_DIR := $(HOME)/.config/micro/plug/agent
BIN_DIR := $(HOME)/.local/bin

.PHONY: build clean install uninstall test fmt vet

build:
	cd protocol && go build ./...
	mkdir -p server/bin
	cd server && go build -o bin/junto-server ./cmd/junto-server
	mkdir -p bridge/bin
	cd bridge && go build -o bin/junto-bridge ./cmd/junto-bridge

install: build
	@# Install plugin
	mkdir -p $(PLUGIN_DIR)
	ln -sf $(CURDIR)/plugin/main.lua $(PLUGIN_DIR)/main.lua
	ln -sf $(CURDIR)/plugin/repo.json $(PLUGIN_DIR)/repo.json
	@# Install binaries
	mkdir -p $(BIN_DIR)
	ln -sf $(CURDIR)/server/bin/junto-server $(BIN_DIR)/junto-server
	ln -sf $(CURDIR)/bridge/bin/junto-bridge $(BIN_DIR)/junto-bridge
	@echo "Installed. Ensure $(BIN_DIR) is in PATH."
	@echo "Start server: junto-server"
	@echo "In micro: :junto <socket-path>"

uninstall:
	rm -f $(BIN_DIR)/junto-server $(BIN_DIR)/junto-bridge
	rm -rf $(PLUGIN_DIR)

test:
	cd protocol && go test ./...
	cd server && go test ./...
	cd bridge && go test ./...

fmt:
	cd protocol && go fmt ./...
	cd server && go fmt ./...
	cd bridge && go fmt ./...

vet:
	cd protocol && go vet ./...
	cd server && go vet ./...
	cd bridge && go vet ./...

clean:
	rm -f server/bin/* bridge/bin/*
