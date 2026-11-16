// Package usage reports each tenant's usage counts to Kora Cloud.
//
// Every few hours a tenant worker counts the tenant's records and posts one
// snapshot to Cloud's usage endpoint (POST /api/cloud/usage). Cloud keeps the
// latest snapshot per site and checks plan limits against it. Reporting is
// optional: it runs only when KORA_CLOUD_USAGE_URL is set.
package usage
