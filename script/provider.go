package script

import "net/http"

// KoraProvider is the bridge between the JavaScript runtime and the Kora engine.
// The engine implements this interface and passes it via ExecuteRequest.
// Scripts call kora.getDoc() → provider.GetDoc().
type KoraProvider interface {
	// Document CRUD — operates within the triggering user's permissions.
	GetDoc(doctype, name string) (map[string]any, error)
	GetList(doctype string, filters map[string]any, orderBy string, limit, offset int) ([]map[string]any, error)
	SaveDoc(doctype string, doc map[string]any, modifiedBy string) error
	CreateDoc(doctype string, doc map[string]any, owner, modifiedBy string) (map[string]any, error)
	DeleteDoc(doctype, name string) error

	// Secrets — returns decrypted value from _kora_secret.
	GetSecret(key string) (string, error)

	// HTTP — makes external HTTP requests (subject to domain allowlist).
	DoHTTP(req *HTTPRequest) (*HTTPResponse, error)
}

// HTTPRequest represents an outgoing HTTP request from a script.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
}

// HTTPResponse represents an HTTP response returned to a script.
type HTTPResponse struct {
	Status     int
	StatusText string
	Headers    map[string]string
	Body       []byte
}

// HTTPClient is the interface for making external HTTP requests from scripts.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// NoopProvider is a safe default that returns errors for all operations.
// Used when the script runner runs without engine integration (tests, validation).
type NoopProvider struct{}

func (NoopProvider) GetDoc(doctype, name string) (map[string]any, error) {
	return nil, nil
}
func (NoopProvider) GetList(doctype string, filters map[string]any, orderBy string, limit, offset int) ([]map[string]any, error) {
	return nil, nil
}
func (NoopProvider) SaveDoc(doctype string, doc map[string]any, modifiedBy string) error {
	return nil
}
func (NoopProvider) CreateDoc(doctype string, doc map[string]any, owner, modifiedBy string) (map[string]any, error) {
	return nil, nil
}
func (NoopProvider) DeleteDoc(doctype, name string) error {
	return nil
}
func (NoopProvider) GetSecret(key string) (string, error) {
	return "", nil
}
func (NoopProvider) DoHTTP(req *HTTPRequest) (*HTTPResponse, error) {
	return nil, nil
}
