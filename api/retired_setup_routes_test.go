package api

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/orm"
)

// The AI workspace-setup interview (tenant /cloud/conversations) was retired
// with its onboarding screen. Its routes must stay unregistered; Cloud owns the
// onboarding conversation on its own host.
func TestRetiredWorkspaceSetupConversationRoutesAreNotRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	for _, prefix := range []string{"/api/v1", "/api"} {
		RegisterRoutesOnGroupWithRuntimeServices(router.Group(prefix), nil, &orm.TxManager{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	}
	for _, route := range router.Routes() {
		if strings.Contains(route.Path, "/cloud/conversations") {
			t.Errorf("retired route still registered: %s %s", route.Method, route.Path)
		}
	}
}
