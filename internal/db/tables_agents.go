package db

// AgentTables returns tenant-scoped runtime tables for generic agent
// manifests and durable run/handoff records. JSON remains flexible while the
// ownership and timestamps stay queryable.
func AgentTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_agent_manifest (id VARCHAR(191) PRIMARY KEY, manifest_json TEXT NOT NULL, updated_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_agent_run (id VARCHAR(191) PRIMARY KEY, agent_id VARCHAR(191) NOT NULL, run_json TEXT NOT NULL, created_at DATETIME(6) NOT NULL, INDEX idx_agent_run_agent (agent_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
}
func AgentTablesLibSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_agent_manifest (id TEXT PRIMARY KEY, manifest_json TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS _kora_agent_run (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, run_json TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_run_agent ON _kora_agent_run (agent_id)`,
	}
}
func AgentTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS "_kora_agent_manifest" ("id" VARCHAR(191) PRIMARY KEY, "manifest_json" TEXT NOT NULL, "updated_at" TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS "_kora_agent_run" ("id" VARCHAR(191) PRIMARY KEY, "agent_id" VARCHAR(191) NOT NULL, "run_json" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_run_agent ON "_kora_agent_run" ("agent_id")`,
	}
}
