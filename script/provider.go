package script

// DocProvider is the interface for document CRUD operations.
// Consumers that only need document access should accept this interface.
type DocProvider interface {
	GetDoc(doctype, name string) (map[string]any, error)
	GetList(doctype string, filters map[string]any, orderBy string, limit, offset int) ([]map[string]any, error)
	SaveDoc(doctype string, doc map[string]any, modifiedBy string) error
	CreateDoc(doctype string, doc map[string]any, owner, modifiedBy string) (map[string]any, error)
	DeleteDoc(doctype, name string) error
}

// SecretProvider is the interface for retrieving secrets.
type SecretProvider interface {
	GetSecret(key string) (string, error)
}

// HTTPProvider is the interface for making external HTTP requests.
type HTTPProvider interface {
	DoHTTP(req *HTTPRequest) (*HTTPResponse, error)
}

// KoraProvider is the composed interface for backward compatibility.
type KoraProvider interface {
	DocProvider
	SecretProvider
	HTTPProvider
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
