package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

type gateErrorResponse struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func TestRequireSafeDoctypeChange(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name         string
		oldDocTypes  []*doctype.DocType
		newDocTypes  []*doctype.DocType
		force        bool
		wantAllowed  bool
		wantStatus   int
		wantTier     string
		wantCode     string
		wantContains string
	}{
		{
			name: "add optional field is allowed",
			oldDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}},
			},
			newDocTypes: []*doctype.DocType{
				{
					Name: "Task",
					Fields: []doctype.Field{
						{Fieldname: "title", Fieldtype: "Data"},
						{Fieldname: "notes", Fieldtype: "Text"},
					},
				},
			},
			wantAllowed: true,
		},
		{
			name: "remove field is blocked",
			oldDocTypes: []*doctype.DocType{
				{
					Name: "Task",
					Fields: []doctype.Field{
						{Fieldname: "title", Fieldtype: "Data"},
						{Fieldname: "notes", Fieldtype: "Text"},
					},
				},
			},
			newDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}},
			},
			wantAllowed:  false,
			wantStatus:   http.StatusConflict,
			wantCode:     "doctype.change_requires_confirmation",
			wantTier:     "warning",
			wantContains: "remove-field",
		},
		{
			name: "change field type is blocked",
			oldDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}},
			},
			newDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Int"}}},
			},
			wantAllowed:  false,
			wantStatus:   http.StatusConflict,
			wantCode:     "doctype.change_requires_confirmation",
			wantTier:     "blocked",
			wantContains: "change-field-type",
		},
		{
			name: "rename via renamed_from is allowed",
			oldDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "status", Fieldtype: "Data"}}},
			},
			newDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "state", Fieldtype: "Data", RenamedFrom: "status"}}},
			},
			wantAllowed: true,
		},
		{
			name: "force override allows destructive change",
			oldDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}},
			},
			newDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{}},
			},
			force:       true,
			wantAllowed: true,
		},
		{
			name: "add required field without default is blocked",
			oldDocTypes: []*doctype.DocType{
				{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}},
			},
			newDocTypes: []*doctype.DocType{
				{
					Name: "Task",
					Fields: []doctype.Field{
						{Fieldname: "title", Fieldtype: "Data"},
						{Fieldname: "owner", Fieldtype: "Link", Reqd: true},
					},
				},
			},
			wantAllowed:  false,
			wantStatus:   http.StatusConflict,
			wantCode:     "doctype.change_requires_confirmation",
			wantTier:     "warning",
			wantContains: "add-field",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			path := "/api/system/doctype"
			if tt.force {
				path += "?force=true"
			}
			c.Request = httptest.NewRequest(http.MethodPost, path, nil)

			got := requireSafeDoctypeChange(c, tt.oldDocTypes, tt.newDocTypes)
			if got != tt.wantAllowed {
				t.Fatalf("requireSafeDoctypeChange() = %v, want %v", got, tt.wantAllowed)
			}

			if tt.wantAllowed {
				if w.Code != 200 {
					t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
				}
				return
			}

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			var resp gateErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error.Code != tt.wantCode {
				t.Fatalf("error code = %q, want %q", resp.Error.Code, tt.wantCode)
			}
			if tt.wantTier != "" {
				if gotTier, _ := resp.Error.Details["impact_tier"].(string); gotTier != tt.wantTier {
					t.Fatalf("impact_tier = %q, want %q", gotTier, tt.wantTier)
				}
			}
			if tt.wantContains != "" {
				body := w.Body.String()
				if !strings.Contains(body, tt.wantContains) {
					t.Fatalf("response body %q does not contain %q", body, tt.wantContains)
				}
			}
		})
	}
}
