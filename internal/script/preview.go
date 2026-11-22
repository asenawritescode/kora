package script

import (
	"context"
	"fmt"
)

// previewKey marks a context whose script runs belong to a preview.
type previewKey struct{}

// WithPreview returns ctx marked as a preview. A script that runs under it may not send
// outbound HTTP: the write it belongs to is only being checked, so an external effect must not happen.
func WithPreview(ctx context.Context) context.Context {
	return context.WithValue(ctx, previewKey{}, true)
}

// IsPreview reports whether ctx is a preview context.
func IsPreview(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	preview, _ := ctx.Value(previewKey{}).(bool)
	return preview
}

// previewHTTPProvider refuses every outbound request of a preview run.
type previewHTTPProvider struct{}

// DoHTTP refuses the request: the write is only being checked, so no external effect may happen.
func (previewHTTPProvider) DoHTTP(*HTTPRequest) (*HTTPResponse, error) {
	return nil, fmt.Errorf("kora.http.fetch is not allowed while the change is being previewed; it runs only after the person confirms")
}

// outboundHTTPProvider returns the HTTP provider a run uses: the refusing one under a preview,
// otherwise the run's own provider (nil when it has none).
func outboundHTTPProvider(ctx context.Context, provider KoraProvider) HTTPProvider {
	if IsPreview(ctx) {
		return previewHTTPProvider{}
	}
	if provider == nil {
		return nil
	}
	return provider
}
