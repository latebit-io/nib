package ui

import (
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
		return ""
	}
	return string(out)
}

// clipboardWrite writes to the system clipboard.
func clipboardWrite(s string) {
	cmd := clipboardWriteCmd()
	if cmd == nil {
		return
	}
	cmd.Stdin = strings.NewReader(s)
	_ = cmd.Run()
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
