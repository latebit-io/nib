package ui

// ClipboardService provides system clipboard access.
// Panes depend on this interface, not the concrete implementation,
// making them testable with mock clipboard.
type ClipboardService interface {
	// Read returns the current clipboard text ("" when empty or unavailable).
	Read() string
	// Write replaces the clipboard contents.
	Write(s string) error
}

// Services holds shared services available to all panes.
// Passed via constructor injection — not a service locator.
type Services struct {
	// Clipboard is the system clipboard used for copy/cut/paste.
	Clipboard ClipboardService
}

// systemClipboard implements ClipboardService using OS-specific commands.
type systemClipboard struct{}

func (c *systemClipboard) Read() string         { return clipboardRead() }
func (c *systemClipboard) Write(s string) error { return clipboardWrite(s) }

// NewServices creates services with system defaults.
func NewServices() *Services {
	return &Services{
		Clipboard: &systemClipboard{},
	}
}
