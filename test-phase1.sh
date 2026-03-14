#!/bin/bash
# test-phase1.sh — Launch junto server + Micro editor for Phase 1 testing
# Usage: ./test-phase1.sh

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_DIR"

# Ensure binaries are built
echo "Building..."
if ! make build > /dev/null 2>&1; then
    echo "Error: make build failed. Aborting."
    exit 1
fi

# Add ~/.local/bin to PATH if not already there
export PATH="$HOME/.local/bin:$PATH"

# Verify junto-bridge is available
if ! command -v junto-bridge &> /dev/null; then
    echo "Error: junto-bridge not in PATH"
    echo "Run: make install"
    exit 1
fi

# Kill any stale junto-server processes (only those started from this repo)
pkill -f "$REPO_DIR/server/bin/junto-server" 2>/dev/null || true
sleep 0.3

# Clean up stale socket files (restricted to system temp directory)
rm -f "${TMPDIR:-/tmp}"/junto-*.sock 2>/dev/null || true

# Create a temporary test file
TEST_FILE="/tmp/test-junto.go"
cat > "$TEST_FILE" << 'EOF'
package main

import "fmt"

func main() {
	fmt.Println("Hello, Junto!")
}
EOF

cleanup() {
    kill "${SERVER_PID:-}" 2>/dev/null || true
    rm -f "$TEST_FILE" /tmp/junto-server.out 2>/dev/null || true
    rm -f "${TMPDIR:-/tmp}"/junto-*.sock 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Start the server in the background and capture output
echo "Starting junto-server..."
$REPO_DIR/server/bin/junto-server > /tmp/junto-server.out 2>&1 &
SERVER_PID=$!

# Wait for server to write socket path
sleep 0.5

# Extract socket path from server output
SOCKET_PATH=$(head -1 /tmp/junto-server.out | grep -oE '/[^ ]+\.sock')

if [ -z "$SOCKET_PATH" ]; then
    echo "Error: Failed to start server or get socket path"
    echo "Server output:"
    cat /tmp/junto-server.out
    exit 1
fi

echo "✓ Server started (PID $SERVER_PID)"
echo "✓ Socket: $SOCKET_PATH"
echo ""

# Display instructions before launching Micro
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "TESTING PHASE 1 — Mechanical Loop"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "✓ Server running (PID $SERVER_PID)"
echo "✓ File: $TEST_FILE"
echo ""
echo "📋 SOCKET PATH (copy this):"
echo ""
echo "    $SOCKET_PATH"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "📝 TO TEST:"
echo "  1. After you press ENTER, Micro will open..."
echo "  2. Press Ctrl+E (command mode)"
echo "  3. Type: agent-start $SOCKET_PATH"
echo "  4. Press Enter"
echo ""
echo "✅ EXPECTED BEHAVIOR:"
echo "  • Text appears at line 3: '// TODO: implement Verifier interface'"
echo "  • InfoBar prompt: 'Agent: insert at line 3 ... approve? (y/n)'"
echo "  • Press 'y' to approve (text stays)"
echo "  • Press 'n' to reject (text is undone)"
echo ""
echo "🐛 DEBUG COMMANDS (in Micro Ctrl+E):"
echo "  agent-stop               — disconnect bridge (run before exiting Micro!)"
echo "  agent-send {JSON}        — send raw JSON to server"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Press ENTER to launch Micro..."
read

echo "Launching Micro..."
micro "$TEST_FILE"

echo ""
echo "Done."
