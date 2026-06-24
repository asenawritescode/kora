package doctype

import (
	"fmt"
	"testing"
)

// =============================================================================
// YC Technical Due Diligence — Risks Not Yet Addressed
// =============================================================================
// These are the questions a YC partner or technical reviewer would ask.
// They go beyond code bugs — they're about architectural limits, security
// boundaries, and failure modes that could kill the company.

// ---- DD-1: Multi-tenancy isolation (LibSQL) ----------------------------------

func TestYC_DD1_LibSQL_MultiTenancyIsolation(t *testing.T) {
	// MySQL: each site = separate DATABASE. Strong isolation.
	// LibSQL: all sites share ONE remote database. Isolation is via WHERE site = ? clauses.
	// A single missing WHERE clause = cross-site data leak.

	t.Log("RISK: In LibSQL mode, all sites share a single database.")
	t.Log("  MySQL: CREATE DATABASE per site → GRANT per site → physical isolation")
	t.Log("  LibSQL: one database, all tables have 'site' column, isolation via WHERE clauses")
	t.Log("")
	t.Log("  Every query MUST include 'WHERE site = ?'. One missing clause and")
	t.Log("  site A sees site B's data. There is NO database-level enforcement.")
	t.Log("  No row-level security. No Postgres-like RLS policies.")
	t.Log("  This is a single missing WHERE clause away from a data leak.")
	t.Log("")
	t.Log("  System tables at risk:")
	t.Log("    _kora_analytics_daily  — shared across all sites")
	t.Log("    _kora_analytics_metric — shared across all sites")
	t.Log("    _kora_config_version   — shared across all sites")
	t.Log("    _kora_secret           — shared across all sites (ENCRYPTED, but still)")
	t.Log("")
	t.Log("YC QUESTION: 'How do you guarantee tenant isolation in LibSQL mode?'")
	t.Log("CURRENT ANSWER: Code review and hope. No automated enforcement.")
}

// ---- DD-2: No automated backups or point-in-time recovery ---------------------

func TestYC_DD2_NoBackups(t *testing.T) {
	t.Log("RISK: No backup mechanism exists in Kora itself.")
	t.Log("  - No scheduled DB dumps")
	t.Log("  - No point-in-time recovery")
	t.Log("  - No snapshot before migration")
	t.Log("  - The DROP TABLE in cleanup=full is irreversible")
	t.Log("  - mysqldump/libsql backup is the customer's responsibility")
	t.Log("")
	t.Log("  Combined with the monolithic versioning: if a customer activates")
	t.Log("  the wrong draft and drops 4 doctypes with cleanup=full, there is")
	t.Log("  NO recovery path. The data is gone.")
	t.Log("")
	t.Log("YC QUESTION: 'A customer accidentally deletes their Invoice table.")
	t.Log("  How do they recover?'")
	t.Log("CURRENT ANSWER: Hope they had external backups. Kora provides nothing.")
}

// ---- DD-3: DDL is not transactional in MySQL ----------------------------------

func TestYC_DD3_DDLNotTransactional(t *testing.T) {
	t.Log("RISK: schema.ApplyDDL executes statements one by one. MySQL DDL")
	t.Log("  is mostly non-transactional (implicit COMMIT before each DDL).")
	t.Log("  If migration has 5 ALTER TABLE statements and #3 fails:")
	t.Log("    - #1 and #2 are already committed (columns added)")
	t.Log("    - #3 failed (e.g., duplicate column name after partial rename)")
	t.Log("    - #4 and #5 never run")
	t.Log("  Result: schema is in an inconsistent state. Half-migrated.")
	t.Log("")
	t.Log("  The migrator has no rollback path. No 'undo' DDL is generated.")
	t.Log("  The user sees an error but their DB schema doesn't match the registry.")
	t.Log("")
	t.Log("  LibSQL/SQLite: DDL CAN be transactional. But the code doesn't wrap")
	t.Log("  it in a BEGIN/COMMIT. Each statement auto-commits.")
	t.Log("")
	t.Log("YC QUESTION: 'What happens if a migration fails halfway through?'")
	t.Log("CURRENT ANSWER: Schema is inconsistent. Manual DBA intervention required.")
}

// ---- DD-4: AI has direct write access, no confirmation gate ------------------

func TestYC_DD4_AIDirectWriteAccess(t *testing.T) {
	t.Log("RISK: AI chat's executeToolCallsForAI calls the ORM directly.")
	t.Log("  Tools: *_create, *_update, *_delete — full CRUD.")
	t.Log("  AI operates with the authenticated user's permissions.")
	t.Log("  If the AI misinterprets a prompt or hallucinates an action,")
	t.Log("  it can create/update/delete real data.")
	t.Log("")
	t.Log("  Example: Customer asks 'Clean up old Work Orders from last year'")
	t.Log("  AI could call work_order_delete on hundreds of records.")
	t.Log("  There is NO confirmation gate for destructive AI actions.")
	t.Log("  No 'AI is about to delete 47 records. Confirm?' prompt.")
	t.Log("")
	t.Log("  The only safety net: AI-created records have modified_by='ai-assistant'")
	t.Log("  for audit. But that's post-hoc — the damage is already done.")
	t.Log("")
	t.Log("YC QUESTION: 'Can the AI delete all my data?'")
	t.Log("CURRENT ANSWER: Yes, if the user has delete permissions and the AI")
	t.Log("  decides to. No confirmation, no rate limit on AI actions.")
}

// ---- DD-5: Encryption key = DB password ---------------------------------------

func TestYC_DD5_EncryptionKeyEqualsDBPassword(t *testing.T) {
	t.Log("RISK: secret.Store derives AES-256-GCM key from site DB password via HKDF.")
	t.Log("  If DB password is weak (default: 'kora123' for MySQL dev),")
	t.Log("  the encryption is weak. Key rotation requires changing the DB password,")
	t.Log("  which requires restarting the server with new DB_DSN.")
	t.Log("")
	t.Log("  Secrets stored: AI provider API keys (OpenAI, DeepSeek, Anthropic).")
	t.Log("  If the DB password is compromised, all API keys are compromised.")
	t.Log("  If an API key is compromised, there's no automatic key rotation.")
	t.Log("")
	t.Log("  No key derivation salt stored separately. No KMS integration.")
	t.Log("  No envelope encryption. The master key IS the DB password.")
	t.Log("")
	t.Log("YC QUESTION: 'If someone gets your DB password, do they get all your")
	t.Log("  AI provider keys too?'")
	t.Log("CURRENT ANSWER: Yes. The DB password IS the encryption key.")
}

// ---- DD-6: System console — single credentials, no MFA ------------------------

func TestYC_DD6_SingleSystemCredentials_NoMFA(t *testing.T) {
	t.Log("RISK: System console auth uses a single YAML file (system_credentials.yaml)")
	t.Log("  with one email/password pair. One credential for the entire deployment.")
	t.Log("  No MFA, no role-based access, no audit log for console actions.")
	t.Log("")
	t.Log("  If this credential is compromised, the attacker can:")
	t.Log("    - View ALL sites' data")
	t.Log("    - Delete sites")
	t.Log("    - Change any configuration")
	t.Log("    - Export all data")
	t.Log("")
	t.Log("  The console gives SYSTEM-LEVEL access across all sites.")
	t.Log("  This is a single point of security failure.")
	t.Log("")
	t.Log("YC QUESTION: 'How many people have the console password? What happens")
	t.Log("  if an employee leaves?'")
	t.Log("CURRENT ANSWER: Change the YAML file and restart. No rotation history.")
}

// ---- DD-7: Single process, no per-site resource isolation ---------------------

func TestYC_DD7_SingleProcess_NoSiteIsolation(t *testing.T) {
	t.Log("RISK: All sites run in a single Go process.")
	t.Log("  - A goroutine leak in one site's analytics worker affects all sites")
	t.Log("  - A memory leak from a large AI chat context affects all sites")
	t.Log("  - A crash in any goroutine without recover() brings down all sites")
	t.Log("  - The rate limiter is per-user, not per-site. One site can starve others.")
	t.Log("")
	t.Log("  The analytics worker runs in a goroutine WITHOUT panic recovery:")
	t.Log("    go siteWorker.Start()  // ← no defer/recover in Start()")
	t.Log("  If the worker panics (nil pointer, type assertion failure),")
	t.Log("  the entire binary crashes. ALL sites go down.")
	t.Log("")
	t.Log("YC QUESTION: 'If one customer's site triggers a bug, do other")
	t.Log("  customers go down too?'")
	t.Log("CURRENT ANSWER: Yes. Single process, shared fate.")
}

// ---- DD-8: No audit log -------------------------------------------------------

func TestYC_DD8_NoAuditLog(t *testing.T) {
	t.Log("RISK: No centralized audit log for security-relevant events.")
	t.Log("  Config changes are versioned (who activated what) but:")
	t.Log("    - Who VIEWED sensitive data? Not logged.")
	t.Log("    - Who EXPORTED config? Not logged beyond the version label.")
	t.Log("    - Who DELETED a document? Only in analytics events (if enabled).")
	t.Log("    - Who queried the AI? Logged via slog (stdout), not persisted.")
	t.Log("    - Failed login attempts? bcrypt.CompareHashAndPassword runs,")
	t.Log("      but there's no rate limiting or account lockout.")
	t.Log("")
	t.Log("  SOC2/ISO27001 require audit trails. Kora has none.")
	t.Log("")
	t.Log("YC QUESTION: 'How do you detect a data breach? How do you investigate")
	t.Log("  who accessed what and when?'")
	t.Log("CURRENT ANSWER: You can't. There's no audit trail.")
}

// ---- DD-9: Supply chain — no SBOM, minimal CI checks --------------------------

func TestYC_DD9_SupplyChainMinimalCI(t *testing.T) {
	t.Log("RISK: CI runs go vet + tsc --noEmit + go build.")
	t.Log("  No vulnerability scanning (govulncheck, npm audit).")
	t.Log("  No SBOM generation.")
	t.Log("  No dependency license check.")
	t.Log("  No container image scanning.")
	t.Log("  The UI embeds via go:embed — npm dependencies are baked into the binary.")
	t.Log("")
	t.Log("YC QUESTION: 'If a critical CVE is found in a Go dependency or npm")
	t.Log("  package, how long does it take you to know and patch?'")
	t.Log("CURRENT ANSWER: Manual. No automated vulnerability alerting.")
}

// ---- DD-10: EventBus WAL — single disk file, no rotation ----------------------

func TestYC_DD10_WALFile_RiskOfCorruption(t *testing.T) {
	t.Log("RISK: The analytics WAL is a single JSONL file on disk.")
	t.Log("  - No size-based rotation. File grows indefinitely if worker stalls.")
	t.Log("  - Binary crash mid-write → partially written JSON line → DrainWAL")
	t.Log("    fails on that line, potentially losing all events after it.")
	t.Log("  - Multiple sites write to the SAME WAL directory? Or separate?")
	t.Log("    If same directory: all sites share one WAL file → site A's events")
	t.Log("    could be replayed into site B on recovery.")
	t.Log("")
	t.Log("  Checking the code: NewChannelBus takes a walDir parameter.")
	t.Log("  If two sites get the same walDir, they share a WAL file.")
	t.Log("  Is walDir unique per site? Need to verify in cli/serve.go.")
	t.Log("")
	t.Log("YC QUESTION: 'If the server crashes during peak load, how many")
	t.Log("  analytics events are lost? Can they be recovered?'")
	t.Log("CURRENT ANSWER: WAL replays on startup but there are edge cases.")
}

// ---- DD-11: No soft-delete for documents ---------------------------------------

func TestYC_DD11_NoSoftDelete(t *testing.T) {
	t.Log("RISK: orm/query.go DELETE is a real DELETE FROM.")
	t.Log("  No deleted_at column. No trash/recycle bin. No undo.")
	t.Log("  Combined with AI write access: AI could delete critical records")
	t.Log("  and there's no recovery path.")
	t.Log("")
	t.Log("  Even without AI: a user with delete permission who makes a mistake")
	t.Log("  has no undo. The document is gone.")
	t.Log("")
	t.Log("YC QUESTION: 'A customer's employee accidentally deletes a Customer")
	t.Log("  record with 200 linked Invoices. What happens to the Invoices?'")
	t.Log("CURRENT ANSWER: The ORM cascades? Or orphans them? Need to verify.")
	t.Log("  Either way, the Customer is irrecoverable without backups.")
}

// ---- DD-12: Analytics retention = data loss by design -------------------------

func TestYC_DD12_AnalyticsRetention_DataLoss(t *testing.T) {
	t.Log("RISK: analytics.Config defaults to RetentionDays=30.")
	t.Log("  cleanupRetention() at worker.go:417 deletes rows older than 30 days.")
	t.Log("  This means: trend charts can NEVER show more than 30 days of history.")
	t.Log("  Monthly rollups exist but also get cleaned after 30 days.")
	t.Log("")
	t.Log("  If a customer wants a year-over-year comparison: impossible.")
	t.Log("  The data is GONE. Not archived, not moved to cold storage — deleted.")
	t.Log("")
	t.Log("  For a YC company: analytics is one of the key value props.")
	t.Log("  Deleting analytics data after 30 days undercuts the pitch.")
	t.Log("")
	t.Log("YC QUESTION: 'Can I see my sales trend over the last 12 months?'")
	t.Log("CURRENT ANSWER: Not with default settings. Data is deleted after 30 days.")
}

// ---- DD-13: Go panic in analytics worker = process crash -----------------------

func TestYC_DD13_WorkerPanic_CrashesProcess(t *testing.T) {
	t.Log("RISK: The analytics worker runs in a goroutine without panic recovery.")
	t.Log("  cli/serve.go:160: go siteWorker.Start()")
	t.Log("  Start() at worker.go:70 has NO defer/recover block.")
	t.Log("")
	t.Log("  If the worker panics (nil DB dereference, type assertion on event.Data,")
	t.Log("  race condition in the delta map), the panic propagates to the top-level")
	t.Log("  goroutine and CRASHES THE ENTIRE PROCESS.")
	t.Log("")
	t.Log("  All sites go down. The HTTP server stops. No graceful degradation.")
	t.Log("  One bad analytics event can take down the entire platform.")
	t.Log("")
	t.Log("  Note: gin.Recovery() only catches panics in HTTP handler goroutines.")
	t.Log("  Background goroutines need their own recover(). The worker has none.")
	t.Log("")
	t.Log("YC QUESTION: 'What happens if your analytics pipeline hits a nil pointer?'")
	t.Log("CURRENT ANSWER: The binary crashes. All customers go offline.")
}

// ---- DD-14: Password hashing with bcrypt — no cost factor configurability ------

func TestYC_DD14_BcryptCost_NotConfigurable(t *testing.T) {
	t.Log("RISK: Session auth uses bcrypt for password hashing (good).")
	t.Log("  But if the cost factor is the default (10), it may be too fast")
	t.Log("  on modern hardware — vulnerable to brute force.")
	t.Log("  Combined with no rate limiting on login: attacker can try many passwords.")
	t.Log("")
	t.Log("  No account lockout after N failed attempts.")
	t.Log("  No password complexity requirements enforced by the server.")
	t.Log("")
	t.Log("YC QUESTION: 'How do you protect against credential stuffing?'")
	t.Log("CURRENT ANSWER: Rate limiter exists but is per-user, not per-endpoint.")
	t.Log("  /api/auth/login could be hammered.")
}

// ---- DD-15: No data export for customers (vendor lock-in) ----------------------

func TestYC_DD15_NoCustomerDataExport(t *testing.T) {
	t.Log("RISK: There's kora config export (exports CONFIG, not data).")
	t.Log("  There is NO 'export all my data' endpoint for customers.")
	t.Log("  No CSV export. No API endpoint to dump all documents from a doctype.")
	t.Log("  If a customer wants to leave Kora, they have to:")
	t.Log("    1. Write custom SQL queries against the DB")
	t.Log("    2. Or use the list API with pagination and manually reconstruct")
	t.Log("")
	t.Log("  This is a vendor lock-in red flag for enterprise sales.")
	t.Log("  GDPR requires data portability. SOC2 expects export capability.")
	t.Log("")
	t.Log("YC QUESTION: 'How does a customer get their data out?'")
	t.Log("CURRENT ANSWER: They can't easily. No export feature exists.")
}

// ---- Summary -----------------------------------------------------------------

func TestYC_Diligence_Summary(t *testing.T) {
	risks := []struct {
		id    string
		title string
		cat   string
		sev   string
	}{
		{"DD1", "LibSQL multi-tenant isolation via WHERE clauses only", "Security", "Critical"},
		{"DD2", "No automated backups or point-in-time recovery", "Reliability", "Critical"},
		{"DD3", "DDL not transactional — partial migration = broken schema", "Reliability", "Critical"},
		{"DD4", "AI has direct write access, no confirmation gate", "Security", "Critical"},
		{"DD5", "Encryption key derived from DB password", "Security", "High"},
		{"DD6", "Single system console credential, no MFA", "Security", "High"},
		{"DD7", "Single process — one site crash = all sites down", "Reliability", "High"},
		{"DD8", "No audit log for security events", "Compliance", "High"},
		{"DD9", "No SBOM, no vuln scanning in CI", "Supply Chain", "Medium"},
		{"DD10", "WAL single-file, no rotation, corruption risk", "Reliability", "Medium"},
		{"DD11", "No soft-delete — real DELETE, no recovery", "Data Integrity", "Medium"},
		{"DD12", "Analytics retention = 30 days, data deleted forever", "Product", "Medium"},
		{"DD13", "Worker panic without recover = process crash", "Reliability", "Critical"},
		{"DD14", "No login rate limiting, no account lockout", "Security", "Medium"},
		{"DD15", "No customer data export — vendor lock-in", "Product", "Medium"},
	}

	fmt.Println("\n=================================================")
	fmt.Println("  YC TECHNICAL DUE DILIGENCE — RISK REGISTER")
	fmt.Println("=================================================")
	for _, r := range risks {
		fmt.Printf("  [%s] [%s] %s: %s\n", r.sev, r.cat, r.id, r.title)
	}
	fmt.Println("=================================================")
	fmt.Printf("  Critical: %d, High: %d, Medium: %d\n",
		countBySevStr(risks, "Critical"),
		countBySevStr(risks, "High"),
		countBySevStr(risks, "Medium"))
	fmt.Println("=================================================")
	fmt.Println("  TOP 3 KILLERS:")
	fmt.Println("  1. Multi-tenant isolation bug → cross-site data leak")
	fmt.Println("  2. Worker panic → all customer sites offline")
	fmt.Println("  3. AI deletes real data → no recovery, no backups")
	fmt.Println("=================================================")
}

func countBySevStr(risks []struct {
	id    string
	title string
	cat   string
	sev   string
}, sev string) int {
	n := 0
	for _, r := range risks {
		if r.sev == sev {
			n++
		}
	}
	return n
}
