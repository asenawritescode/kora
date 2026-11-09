package api

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHealthPayloadIncludesAsyncHookOverflow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(nil)
	payload := healthPayload(c)

	if payload["status"] != "ok" {
		t.Fatalf("status = %v, want ok", payload["status"])
	}
	if payload["db"] != "unknown" {
		t.Fatalf("db = %v, want unknown", payload["db"])
	}
	if _, ok := payload["async_hook_enqueue_failed"]; !ok {
		t.Fatal("missing async_hook_enqueue_failed counter")
	}
}
