package headless

import (
	"encoding/json"
	"io"
)

// WriteJSON encodes the result as indented JSON to w. Specialization
// Results that embed [Result] gain this method through promotion;
// override it to emit additional fields in a deterministic order.
func (r *Result) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
