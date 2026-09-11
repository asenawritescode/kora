package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
)

func inventoryContext(h *Handler, method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Set("site_name", "tenant-1")
	c.Set("user", "operator-1")
	c.Set("site_registry", &doctype.Registry{})
	return c, recorder
}

func TestInventoryMovementAPIProjectsBalanceAndEmitsValidationPath(t *testing.T) {
	h := NewHandler(nil, &orm.TxManager{})
	c, rec := inventoryContext(h, "POST", "/api/v1/inventory/movements", `{"id":"move-1","item":"item-1","location":"warehouse-1","quantity":10,"kind":"receive","reason":"opening"}`)
	h.HandleInventoryMovement(c)
	if rec.Code != 201 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	c, rec = inventoryContext(h, "GET", "/api/v1/inventory/balance?item=item-1&location=warehouse-1", "")
	h.HandleInventoryBalance(c)
	var response struct {
		Data struct {
			Balance   int64 `json:"balance"`
			Available int64 `json:"available"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Balance != 10 || response.Data.Available != 10 {
		t.Fatalf("unexpected balance: %+v", response.Data)
	}
}

func TestInventoryMovementAPIRejectsNegativeStock(t *testing.T) {
	h := NewHandler(nil, &orm.TxManager{})
	c, rec := inventoryContext(h, "POST", "/api/v1/inventory/movements", `{"id":"move-1","item":"item-1","location":"warehouse-1","quantity":-1,"kind":"issue","reason":"issue"}`)
	h.HandleInventoryMovement(c)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
