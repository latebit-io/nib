BIN_DIR := $(HOME)/.local/bin

.PHONY: build clean install uninstall test fmt vet

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
