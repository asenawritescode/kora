package doctype

import (
	"path/filepath"
	"testing"
)

func TestNewTemplateConfigPacksParse(t *testing.T) {
	for _, pack := range []string{"clinic", "logistics", "sacco", "school"} {
		t.Run(pack, func(t *testing.T) {
			packPath := filepath.Join("../config", "v0", pack)
			doctypes, err := ParseConfigTree(packPath)
			if err != nil {
				t.Fatalf("parse config pack: %v", err)
			}
			if len(doctypes) == 0 {
				t.Fatalf("expected at least one DocType")
			}
		})
	}
}
