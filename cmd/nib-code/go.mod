module github.com/latebit-io/nib/cmd/nib-code

go 1.26

require (
	charm.land/bubbletea/v2 v2.0.2
	github.com/latebit-io/nib/ai v0.0.0
	github.com/latebit-io/nib/coding v0.0.0
	github.com/latebit-io/nib/engine v0.0.0
	github.com/latebit-io/nib/tui v0.0.0
)

require (
	charm.land/lipgloss/v2 v2.0.2 // indirect
	github.com/charmbracelet/colorprofile v0.4.2 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260205113103-524a6607adb8 // indirect
	github.com/charmbracelet/x/ansi v0.11.6 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/latebit-io/nib/agent v0.0.0 // indirect
	github.com/latebit-io/nib/kit v0.0.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/tree-sitter-grammars/tree-sitter-lua v0.5.0 // indirect
	github.com/tree-sitter-grammars/tree-sitter-yaml v0.7.2 // indirect
	github.com/tree-sitter/go-tree-sitter v0.25.0 // indirect
	github.com/tree-sitter/tree-sitter-go v0.25.0 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace (
	github.com/latebit-io/nib/agent => ../../agent
	github.com/latebit-io/nib/ai => ../../ai
	github.com/latebit-io/nib/coding => ../../coding
	github.com/latebit-io/nib/engine => ../../engine
	github.com/latebit-io/nib/kit => ../../kit
	github.com/latebit-io/nib/tui => ../../tui
)
