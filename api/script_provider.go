package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
)

// scriptProvider bridges the JS runtime to Kora's canonical query and command APIs.
type scriptProvider struct {
	tx          *orm.TxManager
	registry    *doctype.Registry
	site        string
	secretStore *secret.Store

	// HTTP allowlist controls which domains scripts can call.
	HTTPAllowlist []string

	httpClient       *http.Client
	activeHooks      []string
	mutationExecutor script.MutationExecutor
}

// NewScriptProvider creates a provider with a scoped HTTP client.
func NewScriptProvider(tx *orm.TxManager, registry *doctype.Registry, site string, secretStore *secret.Store, httpAllowlist []string) script.KoraProvider {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		MaxIdleConns:    10,
		IdleConnTimeout: 60 * time.Second,
	}
	return &scriptProvider{
		tx:            tx,
		registry:      registry,
		site:          site,
		secretStore:   secretStore,
		HTTPAllowlist: httpAllowlist,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
	}
}

// GetDoc fetches a single document by doctype and name.
func (p *scriptProvider) GetDoc(doctypeName, name string) (map[string]any, error) {
	dt := p.registry.Get(doctypeName)
	if dt == nil {
		return nil, fmt.Errorf("doctype %q not found", doctypeName)
	}
	doc, err := p.tx.GetDoc(dt, name, "")
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, nil
	}
	return doc.ToMap(), nil
}

// GetList fetches documents with optional filters, ordering, and pagination.
func (p *scriptProvider) GetList(doctypeName string, filters map[string]any, orderBy string, limit, offset int) ([]map[string]any, error) {
	dt := p.registry.Get(doctypeName)
	if dt == nil {
		return nil, fmt.Errorf("doctype %q not found", doctypeName)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500 // cap to prevent memory exhaustion
	}

	// Convert map filters to JSON filter string (Kora ORM format).
	filterStr := mapToFilterString(filters)

	docs, _, err := p.tx.GetList(dt, filterStr, orderBy, limit, offset, "")
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, len(docs))
	for i, doc := range docs {
		result[i] = doc.ToMap()
	}
	return result, nil
}

// mapToFilterString converts a map of field→value pairs to JSON filter format.
// Example: {customer: "CUST-0001", status: "Open"} → [["customer","=","CUST-0001"],["status","=","Open"]]
func mapToFilterString(filters map[string]any) string {
	if len(filters) == 0 {
		return ""
	}
	var parts []string
	for k, v := range filters {
		// Support operators via special map keys: {"status": ["!=", "Completed"]}
		op := "="
		val := v
		if arr, ok := v.([]any); ok && len(arr) == 2 {
			if opStr, ok2 := arr[0].(string); ok2 {
				op = opStr
			}
			val = arr[1]
		}
		valStr := fmt.Sprintf("%v", val)
		parts = append(parts, fmt.Sprintf(`["%s","%s","%s"]`, k, op, valStr))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// SaveDoc updates an existing document.
func (p *scriptProvider) SaveDoc(doctypeName string, doc map[string]any, modifiedBy string) error {
	dt := p.registry.Get(doctypeName)
	if dt == nil {
		return fmt.Errorf("doctype %q not found", doctypeName)
	}
	name, ok := doc["name"].(string)
	if !ok || name == "" {
		return fmt.Errorf("document must have a 'name' field")
	}

	data, err := scriptMutationFields(dt, p.registry, doc)
	if err != nil {
		return err
	}
	_, err = p.executeRecordMutation(kernel.CommandRecordUpdate, dt.Name, name, data, modifiedBy, "")
	return err
}

// CreateDoc creates a new document.
func (p *scriptProvider) CreateDoc(doctypeName string, doc map[string]any, owner, modifiedBy string) (map[string]any, error) {
	dt := p.registry.Get(doctypeName)
	if dt == nil {
		return nil, fmt.Errorf("doctype %q not found", doctypeName)
	}

	data, err := scriptMutationFields(dt, p.registry, doc)
	if err != nil {
		return nil, err
	}
	result, err := p.executeRecordMutation(kernel.CommandRecordCreate, dt.Name, "", data, modifiedBy, owner)
	if err != nil {
		return nil, err
	}
	// KoraProvider historically echoes unknown input keys in the immediate
	// create result, although the ORM never persists them. Keep that behavior
	// at this adapter boundary; kernel and stored documents remain schema-only.
	for key, value := range doc {
		if dt.GetField(key) == nil && !isScriptSystemField(key) {
			result[key] = value
		}
	}
	return result, nil
}

// DeleteDoc deletes a document by doctype and name.
func (p *scriptProvider) DeleteDoc(doctypeName, name string) error {
	dt := p.registry.Get(doctypeName)
	if dt == nil {
		return fmt.Errorf("doctype %q not found", doctypeName)
	}
	_, err := p.executeRecordMutation(kernel.CommandRecordDelete, dt.Name, name, nil, p.tx.CurrentUser, "")
	return err
}

func (p *scriptProvider) WithLifecycleHook(_ string, _ script.Event, scriptName string) script.KoraProvider {
	copyProvider := *p
	copyProvider.activeHooks = append(append([]string(nil), p.activeHooks...), scriptName)
	return &copyProvider
}

func (p *scriptProvider) WithMutationExecutor(executor script.MutationExecutor) script.KoraProvider {
	copyProvider := *p
	copyProvider.mutationExecutor = executor
	return &copyProvider
}

func (p *scriptProvider) executeRecordMutation(command, doctypeName, name string, fields json.RawMessage, actorUser, owner string) (map[string]any, error) {
	if p.tx == nil || p.tx.DB == nil || p.registry == nil {
		return nil, fmt.Errorf("script provider is not bound to a site database")
	}
	if strings.TrimSpace(actorUser) == "" {
		actorUser = p.tx.CurrentUser
	}
	if strings.TrimSpace(actorUser) == "" {
		actorUser = "script-runtime"
	}
	roles := append([]string(nil), p.tx.CurrentUserRoles...)
	if len(roles) == 0 && p.tx.CurrentUserRole != "" {
		roles = []string{p.tx.CurrentUserRole}
	}
	if len(roles) == 0 {
		// Lifecycle scripts historically ran with the system's unrestricted ORM
		// capability. Preserve that trusted internal behavior explicitly rather
		// than making an unauthenticated public adapter permissive.
		roles = []string{doctype.AdminRole}
	}

	rawPayload, err := json.Marshal(struct {
		Doctype string          `json:"doctype"`
		Name    string          `json:"name,omitempty"`
		Data    json.RawMessage `json:"data,omitempty"`
	}{Doctype: doctypeName, Name: name, Data: fields})
	if err != nil {
		return nil, fmt.Errorf("encode script record payload: %w", err)
	}

	principalType := contract.PrincipalHuman
	if actorUser == "script-runtime" || actorUser == "system" {
		principalType = contract.PrincipalService
	}
	actor := contract.ActorContext{
		PrincipalID: actorUser, PrincipalType: principalType,
		SubjectUserID: p.tx.CurrentUser, Site: p.site, Roles: roles,
	}
	user := actorUser
	ctx := p.tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	mutation := script.MutationRequest{
		Command: command, Payload: rawPayload, Site: p.site, User: user, Owner: owner,
		PrincipalID: actor.PrincipalID, PrincipalType: string(actor.PrincipalType),
		SubjectUserID: actor.SubjectUserID, UserRole: p.tx.CurrentUserRole, Roles: roles,
		SkipHookScripts: append([]string(nil), p.activeHooks...), AllowReadOnly: true,
	}
	var resultData json.RawMessage
	if p.mutationExecutor != nil {
		resultData, err = p.mutationExecutor.Execute(ctx, mutation)
	} else {
		k := kernel.New(p.tx.Dialect, p.tx.Outbox)
		k.TxManager = p.tx
		var result contract.CommandResult
		result, cerr := k.Execute(ctx, p.tx.DB, p.registry, kernel.Operation{
			Context: kernel.OperationContext{
				Site: p.site, Actor: actor, User: user, Owner: owner,
				UserRole: p.tx.CurrentUserRole, Roles: roles,
				SkipHookScripts:     append([]string(nil), p.activeHooks...),
				AllowReadOnlyFields: true, Source: kernel.SourceIntegration,
			},
			Command: command, Payload: rawPayload,
		})
		if cerr != nil {
			return nil, cerr
		}
		resultData = result.Data
	}
	if err != nil {
		return nil, err
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(resultData, &operation); err != nil {
		return nil, fmt.Errorf("decode script record result: %w", err)
	}
	return operation.Document, nil
}

func scriptMutationFields(dt *doctype.DocType, registry *doctype.Registry, fields map[string]any) (json.RawMessage, error) {
	normalized := normalizeScriptDocumentInput(dt, registry, fields)
	allowed := make(map[string]any, len(normalized))
	for key, value := range normalized {
		field := dt.GetField(key)
		if field == nil || isScriptSystemField(key) {
			continue
		}
		if children, ok := value.([]*doctype.Document); ok {
			rows := make([]any, 0, len(children))
			childDT := registry.Get(field.Options)
			for _, child := range children {
				row := make(map[string]any, len(child.Fields))
				for childField, childValue := range child.Fields {
					if childDT == nil || childDT.GetField(childField) != nil {
						row[childField] = childValue
					}
				}
				rows = append(rows, row)
			}
			allowed[key] = rows
			continue
		}
		allowed[key] = value
	}
	encoded, err := json.Marshal(allowed)
	if err != nil {
		return nil, fmt.Errorf("encode script fields: %w", err)
	}
	return encoded, nil
}

func isScriptSystemField(name string) bool {
	switch name {
	case "name", "creation", "modified", "modified_by", "owner", "doc_status", "revision":
		return true
	default:
		return false
	}
}

// GetSecret returns the decrypted value of a secret from _kora_secret.
func (p *scriptProvider) GetSecret(key string) (string, error) {
	if p.secretStore == nil {
		return "", fmt.Errorf("secret store not available")
	}
	return p.secretStore.Get(p.site, key)
}

// DoHTTP executes an external HTTP request with domain allowlist enforcement.
func (p *scriptProvider) DoHTTP(req *script.HTTPRequest) (*script.HTTPResponse, error) {
	if err := p.checkHTTPAllowlist(req.URL); err != nil {
		return nil, err
	}

	method := req.Method
	if method == "" {
		method = "GET"
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}

	httpReq, err := http.NewRequest(method, req.URL, body)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("Content-Type") == "" && req.Body != "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB max
	if err != nil {
		return nil, fmt.Errorf("http: reading response: %w", err)
	}

	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	return &script.HTTPResponse{
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Headers:    respHeaders,
		Body:       respBody,
	}, nil
}

// checkHTTPAllowlist validates that the URL's host is in the domain allowlist.
func (p *scriptProvider) checkHTTPAllowlist(urlStr string) error {
	if len(p.HTTPAllowlist) == 0 {
		return fmt.Errorf("http: external requests are disabled (no domain allowlist configured)")
	}

	// Strip scheme.
	host := urlStr
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// Strip path.
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	// Strip port.
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)

	// Block private IPs.
	if isPrivateHost(host) {
		return fmt.Errorf("http: requests to private/internal hosts are not allowed")
	}

	// Check against allowlist.
	for _, allowed := range p.HTTPAllowlist {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "*" {
			return nil
		}
		// Exact match.
		if host == allowed {
			return nil
		}
		// Wildcard: *.safaricom.co.ke matches api.safaricom.co.ke.
		if strings.HasPrefix(allowed, "*.") {
			suffix := allowed[1:] // .safaricom.co.ke
			if strings.HasSuffix(host, suffix) {
				return nil
			}
		}
	}

	return fmt.Errorf("http: domain %q is not in the allowed list", host)
}

// isPrivateHost checks if a hostname resolves to or matches a private IP range.
func isPrivateHost(host string) bool {
	// Check hostname patterns.
	privSuffixes := []string{
		".local", ".internal", ".localhost",
	}
	for _, s := range privSuffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	// Check IP ranges.
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
	}
	return false
}

func normalizeScriptDocumentInput(dt *doctype.DocType, registry *doctype.Registry, fields map[string]any) map[string]any {
	if dt == nil || fields == nil {
		return fields
	}
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	for _, field := range dt.TableFields() {
		value, ok := out[field.Fieldname]
		if !ok || value == nil {
			continue
		}
		childDT := registry.Get(field.Options)
		if childDT == nil {
			continue
		}
		children, ok := scriptValueToChildDocuments(value, childDT, registry)
		if ok {
			out[field.Fieldname] = children
		}
	}
	return out
}

func scriptValueToChildDocuments(value any, childDT *doctype.DocType, registry *doctype.Registry) ([]*doctype.Document, bool) {
	switch rows := value.(type) {
	case []*doctype.Document:
		return rows, true
	case []map[string]any:
		children := make([]*doctype.Document, 0, len(rows))
		for _, row := range rows {
			children = append(children, scriptMapToDocument(row, childDT))
		}
		return children, true
	case []any:
		children := make([]*doctype.Document, 0, len(rows))
		for _, item := range rows {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			children = append(children, scriptMapToDocument(row, childDT))
		}
		return children, true
	default:
		return nil, false
	}
}

func scriptMapToDocument(row map[string]any, dt *doctype.DocType) *doctype.Document {
	child := doctype.NewDocument(dt.Name)
	if name, ok := row["name"].(string); ok {
		child.Name = name
		child.IsNew = name == ""
	}
	if status, ok := row["doc_status"]; ok {
		child.DocStatus = intFromScriptValue(status)
	}
	for key, value := range row {
		if key == "name" || key == "doc_status" || key == "owner" || key == "creation" || key == "modified" || key == "modified_by" {
			continue
		}
		child.Set(key, value)
	}
	return child
}

func intFromScriptValue(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func cloneScriptDocument(doc *doctype.Document) *doctype.Document {
	if doc == nil {
		return nil
	}
	clone := &doctype.Document{
		DocType:   doc.DocType,
		Name:      doc.Name,
		Fields:    make(map[string]any, len(doc.Fields)),
		IsNew:     doc.IsNew,
		DocStatus: doc.DocStatus,
	}
	for key, value := range doc.Fields {
		clone.Fields[key] = cloneScriptValue(value)
	}
	return clone
}

func cloneScriptValue(value any) any {
	switch v := value.(type) {
	case []*doctype.Document:
		children := make([]*doctype.Document, len(v))
		for i, child := range v {
			children[i] = cloneScriptDocument(child)
		}
		return children
	case []any:
		items := make([]any, len(v))
		for i, item := range v {
			items[i] = cloneScriptValue(item)
		}
		return items
	case map[string]any:
		m := make(map[string]any, len(v))
		for key, item := range v {
			m[key] = cloneScriptValue(item)
		}
		return m
	default:
		return value
	}
}
