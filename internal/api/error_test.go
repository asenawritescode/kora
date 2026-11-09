package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWriteErrorIncludesCodeMessageAndDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeError(c, http.StatusConflict, "doctype.already_exists", "DocType already exists", map[string]any{
		"doctype": "ApiDupTest",
	})

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	body, ok := resp.Error.(map[string]any)
	if !ok {
		t.Fatalf("error payload type = %T, want map[string]any", resp.Error)
	}
	if body["code"] != "doctype.already_exists" {
		t.Fatalf("code = %v, want doctype.already_exists", body["code"])
	}
	if body["message"] != "DocType already exists" {
		t.Fatalf("message = %v, want DocType already exists", body["message"])
	}
	details, ok := body["details"].(map[string]any)
	if !ok {
		t.Fatalf("details type = %T, want map[string]any", body["details"])
	}
	if details["doctype"] != "ApiDupTest" {
		t.Fatalf("doctype detail = %v, want ApiDupTest", details["doctype"])
	}
}
