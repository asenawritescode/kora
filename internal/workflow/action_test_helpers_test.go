package workflow

import "github.com/asenawritescode/kora/internal/policy"

func testActionGrant() policy.Grant {
	return policy.Grant{Resource: "workflow.action.test", Operation: "submit"}
}
