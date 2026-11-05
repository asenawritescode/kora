package auth

// LookupPasswordLoginAccountForTest and PasswordMatchesForTest expose the
// password sign-in lookup to the external dialect integration tests.
var LookupPasswordLoginAccountForTest = lookupPasswordLoginAccount

func PasswordMatchesForTest(account *passwordLoginAccount, password string) bool {
	return passwordMatches(account, password)
}
