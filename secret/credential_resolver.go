package secret

import (
	"context"
	"fmt"
)

// CredentialResolver adapts this encrypted store to the workflow provider
// dispatch seam. References use key@sha256-encrypted-version and reveal no
// credential material.
type CredentialResolver struct {
	Store *Store
	Site  string
}

func (r CredentialResolver) ResolveCredential(ctx context.Context, value string) (string, error) {
	if ctx == nil || ctx.Err() != nil || r.Store == nil || r.Site == "" {
		return "", fmt.Errorf("credential reference unavailable")
	}
	ref, err := ParseReference(value)
	if err != nil {
		return "", fmt.Errorf("credential reference invalid")
	}
	return r.Store.ResolveReference(r.Site, ref)
}
