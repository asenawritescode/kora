package api

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
)

const (
	importMaxRows  = 5000
	importMaxBytes = 5 << 20
)

var importDecimal = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// parseCSVTable strips a UTF-8 BOM, detects "," or ";" from the header line and
// returns the header and data rows.
func parseCSVTable(raw []byte) ([]string, [][]string, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	first := raw
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		first = raw[:i]
	}
	r := csv.NewReader(bytes.NewReader(raw))
	r.Comma = ','
	if bytes.Count(first, []byte(";")) > bytes.Count(first, []byte(",")) {
		r.Comma = ';'
	}
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("header row required: %w", err)
	}
	var rows [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return header, rows, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("csv: %w", err)
		}
		rows = append(rows, rec)
	}
}

// parseImportValue converts a CSV cell to the JSON value for a field type.
func parseImportValue(f *doctype.Field, s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	switch f.Fieldtype {
	case "Int", "Float", "Currency", "Percent":
		if !importDecimal.MatchString(s) || (f.Fieldtype == "Int" && strings.Contains(s, ".")) {
			return nil, fmt.Errorf("%q is not a valid number", s)
		}
		return s, nil
	case "Date":
		for _, layout := range []string{"2006-01-02", "02/01/2006"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.Format("2006-01-02"), nil
			}
		}
		return nil, fmt.Errorf("%q is not a valid date", s)
	case "Check":
		switch strings.ToLower(s) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
		return nil, fmt.Errorf("%q is not a valid boolean", s)
	}
	return s, nil
}
