package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
)

// koraOps carries out the kora.* calls of one execution, independent of the JS engine.
// The JavaScript binding (koraAPI) only converts arguments and turns an error into a throw.
type koraOps struct {
	req     ExecuteRequest
	logs    []LogEntry
	docs    DocProvider    // nil when req.Provider is nil
	secrets SecretProvider // nil when req.Provider is nil
	http    HTTPProvider   // outboundHTTPProvider(ctx, req.Provider)
}

// newKoraOps builds the operations of one execution from its request and context.
func newKoraOps(ctx context.Context, req ExecuteRequest) *koraOps {
	ops := &koraOps{req: req, logs: make([]LogEntry, 0)}
	if req.Provider != nil {
		ops.docs = req.Provider
		ops.secrets = req.Provider
	}
	ops.http = outboundHTTPProvider(ctx, req.Provider)
	return ops
}

func (o *koraOps) log(level, msg string) {
	o.logs = append(o.logs, LogEntry{Level: level, Message: msg})
}

func (o *koraOps) hasDocs() bool    { return o.docs != nil }
func (o *koraOps) hasSecrets() bool { return o.secrets != nil }
func (o *koraOps) hasHTTP() bool    { return o.http != nil }

// getDoc returns the visible record as a JS value, or (nil, nil) when there is none.
func (o *koraOps) getDoc(doctype, name string) (map[string]any, error) {
	if o.docs == nil {
		return nil, nil
	}
	doc, err := o.docs.GetDoc(doctype, name)
	if err != nil {
		if isEmptyReadError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("kora.getDoc(%q, %q) failed: %w", doctype, name, err)
	}
	if doc == nil {
		return nil, nil
	}
	out, _ := jsValue(doc).(map[string]any) // jsValue of a map is always a map
	return out, nil
}

// getList returns the visible records as a JS array; an empty read is an empty array.
func (o *koraOps) getList(doctype string, opts map[string]any) ([]any, error) {
	if o.docs == nil {
		return []any{}, nil
	}
	filters, _ := opts["filters"].(map[string]any)
	orderBy, _ := opts["orderBy"].(string)
	limit := toInt(opts["limit"], 50)
	offset := toInt(opts["offset"], 0)

	docs, err := o.docs.GetList(doctype, filters, orderBy, limit, offset)
	if err != nil {
		if isEmptyReadError(err) {
			return []any{}, nil
		}
		return nil, fmt.Errorf("kora.getList(%q) failed: %w", doctype, err)
	}
	out, _ := jsValue(docs).([]any) // jsValue of a slice is always a slice
	return out, nil
}

// hasDocType reports whether the record type is installed. Without a DocTypeChecker it is false.
func (o *koraOps) hasDocType(doctype string) bool {
	checker, ok := o.docs.(DocTypeChecker)
	if !ok {
		return false
	}
	return checker.HasDocType(doctype)
}

// saveDoc persists doc as the run's user and returns it as a JS value.
func (o *koraOps) saveDoc(doctype string, doc map[string]any) (any, error) {
	if o.docs == nil {
		return nil, nil
	}
	if err := o.docs.SaveDoc(doctype, doc, o.req.User); err != nil {
		return nil, fmt.Errorf("kora.saveDoc(%q) error: %w", doctype, err)
	}
	return jsValue(doc), nil
}

// createDoc creates doc as the run's user and returns the created record as the provider gave it.
func (o *koraOps) createDoc(doctype string, doc map[string]any) (map[string]any, error) {
	if o.docs == nil {
		return nil, nil
	}
	created, err := o.docs.CreateDoc(doctype, doc, o.req.User, o.req.User)
	if err != nil {
		return nil, fmt.Errorf("kora.createDoc(%q) error: %w", doctype, err)
	}
	return created, nil
}

func (o *koraOps) deleteDoc(doctype, name string) error {
	if o.docs == nil {
		return nil
	}
	if err := o.docs.DeleteDoc(doctype, name); err != nil {
		return fmt.Errorf("kora.deleteDoc(%q, %q) error: %w", doctype, name, err)
	}
	return nil
}

// getSecret returns the secret as a string, or nil when there is no provider or the read
// fails; a failed read is logged as a warning and does not throw.
func (o *koraOps) getSecret(key string) any {
	if o.secrets == nil {
		return nil
	}
	val, err := o.secrets.GetSecret(key)
	if err != nil {
		o.log("warn", fmt.Sprintf("kora.secrets.get error: %v", err))
		return nil
	}
	return val
}

// fetch sends an outbound request. The method defaults to GET and header values are stringified.
func (o *koraOps) fetch(url string, opts map[string]any) (*HTTPResponse, error) {
	if o.http == nil {
		return nil, errors.New("kora.http.fetch: provider not available")
	}
	method, _ := opts["method"].(string)
	if method == "" {
		method = "GET"
	}
	headers, _ := opts["headers"].(map[string]any)
	hdr := make(map[string]string)
	for k, v := range headers {
		hdr[k] = fmt.Sprint(v)
	}
	body, _ := opts["body"].(string)

	resp, err := o.http.DoHTTP(&HTTPRequest{
		Method:  method,
		URL:     url,
		Headers: hdr,
		Body:    body,
	})
	if err != nil {
		return nil, fmt.Errorf("kora.http.fetch error: %w", err)
	}
	return resp, nil
}

// isEmptyReadError reports whether a provider read error means "no visible result"
// (the record does not exist, or this reader may not see it). Those are legitimate
// empty results for a script. Any other error is an infrastructure failure and must throw.
func isEmptyReadError(err error) bool {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Type == contract.CodeNotFound || ce.Type == contract.CodePermissionDenied || ce.Type == contract.CodeUnauthenticated
}

func toInt(v any, defaultVal int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
	case float64:
		return int(n)
	}
	return defaultVal
}
