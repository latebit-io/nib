package nib

import (
	"fmt"

	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/ai/oauth"
)

// ResolveProvider resolves the model profile for dir, attaching the
// token store for a profile that signs in with OAuth. The Resolved
// comes back even with an error, so a host can name what is missing.
func ResolveProvider(dir string) (*llmconfig.Resolved, error) {
	_, resolved := llmconfig.Resolve(dir)
	// Only an OAuth profile opens the store: a corrupt token file must
	// not break a session that uses an API key.
	if resolved.OAuthProvider == "" {
		return resolved, nil
	}
	path, err := oauth.DefaultStorePath()
	if err != nil {
		return resolved, fmt.Errorf("nib: resolve OAuth store path: %w", err)
	}
	store, err := oauth.NewStore(path)
	if err != nil {
		return resolved, fmt.Errorf("nib: open OAuth store at %s: %w", path, err)
	}
	llmconfig.WireOAuth(resolved, store)
	return resolved, nil
}
