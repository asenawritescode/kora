//go:build integration

package api

import (
	"database/sql"
	"net/http"
	"net/url"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// packClient drives a pack loaded into a throwaway MySQL through the resource
// API, the way a workspace user's screens and integrations do.
type packClient struct {
	t        *testing.T
	handler  *Handler
	db       *sql.DB
	registry *doctype.Registry
	site     string
}

func newPackClient(t *testing.T, handler *Handler, db *sql.DB, registry *doctype.Registry, site string) *packClient {
	return &packClient{t: t, handler: handler, db: db, registry: registry, site: site}
}

// send returns the status and the saved document, nil when the call was refused.
func (c *packClient) send(method resourceHTTPMethod, recordType, name, body string) (int, map[string]any) {
	c.t.Helper()
	path := "/api/resource/" + url.PathEscape(recordType)
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	recorder := serveResourceMutationBody(c.t, c.handler, c.db, c.registry, c.site, method, path, body, "", []string{"Administrator"})
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		return recorder.Code, nil
	}
	return recorder.Code, requireRESTDocument(c.t, recorder.Body.Bytes(), recordType)
}

// create saves a record and returns its name; it fails the test if it is refused.
func (c *packClient) create(recordType, body string) string {
	c.t.Helper()
	code, doc := c.send(httpMethodPost, recordType, "", body)
	if doc == nil {
		c.t.Fatalf("create %s = %d: %s", recordType, code, body)
	}
	name, _ := doc["name"].(string)
	return name
}

// refused reports whether saving the record is refused.
func (c *packClient) refused(method resourceHTTPMethod, recordType, name, body string) bool {
	c.t.Helper()
	_, doc := c.send(method, recordType, name, body)
	return doc == nil
}

// value reads one column of one record.
func (c *packClient) value(recordType, name, column string) string {
	c.t.Helper()
	var out sql.NullString
	if err := c.db.QueryRow("SELECT `"+column+"` FROM `tab"+recordType+"` WHERE name = ?", name).Scan(&out); err != nil {
		c.t.Fatalf("%s %s.%s: %v", recordType, name, column, err)
	}
	return out.String
}
