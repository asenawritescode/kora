package doctype

import "fmt"

// Document is the generic runtime representation of any Kora document.
// All documents are stored as map[string]any at runtime.
type Document struct {
	DocType   string
	Name      string
	Fields    map[string]any
	IsNew     bool
	DocStatus int
	Revision  uint64
}

// NewDocument creates a new empty document for the given DocType.
func NewDocument(dtName string) *Document {
	return &Document{
		DocType: dtName,
		Fields:  make(map[string]any),
		IsNew:   true,
	}
}

// Get returns the value of a field, or nil if not set.
func (d *Document) Get(field string) any {
	return d.Fields[field]
}

// Set sets the value of a field.
func (d *Document) Set(field string, value any) {
	d.Fields[field] = value
}

// GetString returns a field value as a string.
func (d *Document) GetString(field string) string {
	v, ok := d.Fields[field]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// GetFloat returns a field value as a float64.
func (d *Document) GetFloat(field string) float64 {
	v, ok := d.Fields[field]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case string:
		var f float64
		fmt.Sscanf(n, "%f", &f)
		return f
	}
	return 0
}

// GetInt returns a field value as an int64.
func (d *Document) GetInt(field string) int64 {
	v, ok := d.Fields[field]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

// GetBool returns a field value as a bool.
func (d *Document) GetBool(field string) bool {
	v, ok := d.Fields[field]
	if !ok || v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

// GetTable returns a Table field value as a slice of child Documents.
func (d *Document) GetTable(field string) []*Document {
	v, ok := d.Fields[field]
	if !ok || v == nil {
		return nil
	}
	docs, _ := v.([]*Document)
	return docs
}

// SetTable sets a Table field value.
func (d *Document) SetTable(field string, docs []*Document) {
	d.Fields[field] = docs
}

// ToMap returns a script/API friendly representation of the document.
// Child table rows are recursively converted from *Document values to maps.
func (d *Document) ToMap() map[string]any {
	if d == nil {
		return nil
	}
	out := make(map[string]any, len(d.Fields)+2)
	for key, value := range d.Fields {
		out[key] = documentValueToMap(value)
	}
	if d.Name != "" {
		out["name"] = d.Name
	}
	out["doc_status"] = d.DocStatus
	out["revision"] = d.Revision
	return out
}

func documentValueToMap(value any) any {
	switch v := value.(type) {
	case []*Document:
		rows := make([]map[string]any, len(v))
		for i, child := range v {
			rows[i] = child.ToMap()
		}
		return rows
	case []any:
		rows := make([]any, len(v))
		for i, item := range v {
			rows[i] = documentValueToMap(item)
		}
		return rows
	case map[string]any:
		m := make(map[string]any, len(v))
		for key, item := range v {
			m[key] = documentValueToMap(item)
		}
		return m
	default:
		return value
	}
}
