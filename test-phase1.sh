#!/bin/bash
# test-junto.sh — Launch junto server + Micro editor for testing
# Usage: ./test-phase1.sh [--stub]
#   --stub    Use hardcoded stub plan (no API key needed)
#   default   Use LLM mode (requires MINIMAX_API_KEY)

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

# Parse flags
SERVER_FLAGS=""
MODE="LLM"
if [[ "${1:-}" == "--stub" ]]; then
    SERVER_FLAGS="--stub"
    MODE="STUB"
fi

# Check for API key in LLM mode
if [[ "$MODE" == "LLM" ]] && [[ -z "${MINIMAX_API_KEY:-}" ]]; then
    echo "Warning: MINIMAX_API_KEY not set. Use --stub for testing without API key."
    echo "  export MINIMAX_API_KEY='your-key-here'"
    echo "  Or: ./test-phase1.sh --stub"
    echo ""
    read -p "Continue anyway? (y/n) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

# Start the server in the background and capture output
echo "Starting junto-server $SERVER_FLAGS..."
$REPO_DIR/server/bin/junto-server $SERVER_FLAGS > /tmp/junto-server.out 2>&1 &
SERVER_PID=$!

# Wait for server to write socket path (poll up to 5s)
SOCKET_PATH=""
for i in $(seq 1 20); do
    if [ -s /tmp/junto-server.out ]; then
        SOCKET_PATH=$(grep -oE '/[^ ]+\.sock' /tmp/junto-server.out | head -n1 || true)
        if [ -n "$SOCKET_PATH" ]; then
            break
        fi
    fi
    sleep 0.25
done

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
echo "TESTING JUNTO — Mode: $MODE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Server running (PID $SERVER_PID)"
echo "File: $TEST_FILE"
echo ""
echo "SOCKET PATH (copy this):"
echo ""
echo "    $SOCKET_PATH"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "TO TEST:"
echo "  1. After you press ENTER, Micro will open..."
echo "  2. Press Ctrl+E (command mode)"
echo "  3. Type: junto $SOCKET_PATH"
echo "  4. Press Enter"
if [[ "$MODE" == "LLM" ]]; then
echo "  5. Type: junto-send add error handling to main"
echo "     (or any goal — this sends the file to the LLM)"
fi
echo ""
echo "EXPECTED BEHAVIOR:"
if [[ "$MODE" == "STUB" ]]; then
echo "  - Agent pane opens, stub plan streams automatically"
echo "  - Step headers: '--- Step 1/3: Add Verifier interface ---'"
echo "  - Code appears char-by-char at agent cursor"
echo "  - Approval prompt: press 'y' to approve, 'n' to reject"
echo "  - After approve: edit freely, then :junto-next to continue"
else
echo "  - Agent pane opens with 'Connected. Send a task with :junto-send'"
echo "  - After :junto-send, LLM reasoning streams in agent pane"
echo "  - Code edits appear char-by-char in code pane"
echo "  - Approval prompt: press 'y' to approve, 'n' to reject"
echo "  - After approve: edit freely, then :junto-next to continue"
fi
echo ""
echo "COMMANDS (Ctrl+E in Micro):"
echo "  junto <socket>           — connect to server"
echo "  junto-send <goal>        — send task to agent (LLM mode)"
echo "  junto-next               — continue to next step (after editing)"
echo "  junto-stop               — disconnect + close agent pane"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Press ENTER to launch Micro..."
read

echo "Launching Micro..."
micro -debug "$TEST_FILE"

echo ""
echo "Done."
