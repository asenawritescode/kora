// Package usage reports each tenant's usage counts to Kora Cloud.
//
// Every few hours a tenant worker measures the tenant's usage and posts one
// snapshot per metric to Cloud's usage endpoint (POST /api/cloud/usage): the
// record count (Records) and the database size in bytes (Storage). Cloud keeps
// the latest snapshot per site and metric and checks plan limits against it.
// Reporting is optional: it runs only when KORA_CLOUD_USAGE_URL is set.
package usage
