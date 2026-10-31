package api

import (
	"strings"

	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

// requestExecution returns only the identity installed by a trusted auth
// adapter. Transport context fields such as user_roles are presentation and
// compatibility data; they cannot establish authority on their own.
func requestExecution(c *gin.Context) (policy.Execution, bool) {
	if c == nil {
		return policy.Execution{}, false
	}
	value, exists := c.Get("policy_execution")
	execution, ok := value.(policy.Execution)
	if !exists || !ok || !execution.Valid() {
		return policy.Execution{}, false
	}
	actor := execution.Actor()
	if site := strings.TrimSpace(c.GetString("site_name")); site != "" && actor.Site != site {
		return policy.Execution{}, false
	}
	if tenant := strings.TrimSpace(c.GetString("site_id")); tenant != "" && string(execution.Tenant()) != tenant {
		return policy.Execution{}, false
	}
	return execution, true
}

func executionUserID(execution policy.Execution) (string, bool) {
	if !execution.Valid() {
		return "", false
	}
	actor := execution.Actor()
	user := strings.TrimSpace(actor.SubjectUserID)
	if user == "" {
		user = strings.TrimSpace(actor.PrincipalID)
	}
	return user, user != ""
}

func requestUserID(c *gin.Context) (string, bool) {
	execution, ok := requestExecution(c)
	if !ok {
		return "", false
	}
	return executionUserID(execution)
}

// scriptIdentity projects only authenticated identity into script context.
// Script-visible roles are informational; authorization remains in the
// verified provider/domain operation boundary.
func scriptIdentity(execution policy.Execution, site string) (string, []string, bool) {
	if !execution.Valid() {
		return "", nil, false
	}
	actor := execution.Actor()
	if actor.Site == "" || actor.Site != site || actor.PrincipalID == "" {
		return "", nil, false
	}
	user, ok := executionUserID(execution)
	if !ok {
		return "", nil, false
	}
	return user, append([]string(nil), actor.Roles...), true
}
