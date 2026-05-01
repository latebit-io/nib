// Package event defines editor-domain events emitted by engine packages
// (e.g. the LSP manager) and consumed by frontends or fan-in layers.
//
// This package is intentionally small: only events that originate from
// pure editor concerns belong here. Application-level events (agent
// lifecycle, edit proposals, status indicators) live in coding/event.
// The wire layer fans this stream into the application's event vocabulary
// so frontends consume a single unified channel.
package event

// Event is the sealed interface for editor-domain events. Only types in
// this package implement it (via the unexported editorEvent marker).
type Event interface {
	editorEvent()
}

// DiagnosticsUpdated signals that diagnostics changed for a file.
// Frontend should re-query the DiagnosticProvider for the updated diagnostics.
type DiagnosticsUpdated struct {
	// Path is the file whose diagnostics changed.
	Path string
}

func (DiagnosticsUpdated) editorEvent() {}
