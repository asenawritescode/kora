package workflow

import "github.com/asenawritescode/kora/policy"

func testActionGrant() policy.Grant {
	return policy.Grant{Resource: "workflow.action.test", Operation: "submit"}
}
