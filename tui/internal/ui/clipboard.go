package ui

import (
	"os/exec"
	"strings"
)

// clipboardRead reads from the system clipboard.
func clipboardRead() string {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// clipboardWrite writes to the system clipboard.
func clipboardWrite(s string) {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(s)
	_ = cmd.Run()
}
