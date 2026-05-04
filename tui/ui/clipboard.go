package ui

import (
	"errors"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
)

// clipboardRead reads from the system clipboard.
func clipboardRead() string {
	cmd := clipboardReadCmd()
	if cmd == nil {
		return ""
	}
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("clipboard read failed", "err", err)
		return ""
	}
	return string(out)
}

// clipboardWrite writes to the system clipboard.
func clipboardWrite(s string) error {
	cmd := clipboardWriteCmd()
	if cmd == nil {
		return errors.New("clipboard not supported on this platform")
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

func clipboardReadCmd() *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("pbpaste")
	case "linux":
		return exec.Command("xclip", "-selection", "clipboard", "-o")
	}
	return nil
}

func clipboardWriteCmd() *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("pbcopy")
	case "linux":
		return exec.Command("xclip", "-selection", "clipboard")
	}
	return nil
}
