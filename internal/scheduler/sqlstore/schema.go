package sqlstore

import "fmt"

func tableStatements(driver string) []string {
	switch driver {
	case "mysql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_schedule_definition (
				tenant_id VARCHAR(140) NOT NULL,
				job_name VARCHAR(140) NOT NULL,
				definition_digest CHAR(64) NOT NULL,
				schedule VARCHAR(255) NOT NULL,
				job_json TEXT NOT NULL,
				next_run_at DATETIME(6) NOT NULL,
				updated_at DATETIME(6) NOT NULL,
				PRIMARY KEY (tenant_id, job_name)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_schedule_occurrence (
				tenant_id VARCHAR(140) NOT NULL,
				job_name VARCHAR(140) NOT NULL,
				scheduled_at DATETIME(6) NOT NULL,
				definition_digest CHAR(64) NOT NULL,
				job_json TEXT NOT NULL,
				state VARCHAR(24) NOT NULL,
				owner VARCHAR(140) NOT NULL DEFAULT '',
				fence BIGINT UNSIGNED NOT NULL DEFAULT 0,
				attempts BIGINT UNSIGNED NOT NULL DEFAULT 0,
				lease_until DATETIME(6),
				finished_at DATETIME(6),
				last_error TEXT NOT NULL,
				PRIMARY KEY (tenant_id, job_name, scheduled_at),
				INDEX idx_schedule_due (tenant_id, job_name, state, scheduled_at),
				INDEX idx_schedule_lease (tenant_id, job_name, state, lease_until),
				INDEX idx_schedule_pending_backlog (tenant_id, state, scheduled_at)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		}
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_schedule_definition (
				tenant_id VARCHAR(140) NOT NULL,
				job_name VARCHAR(140) NOT NULL,
				definition_digest CHAR(64) NOT NULL,
				schedule VARCHAR(255) NOT NULL,
				job_json TEXT NOT NULL,
				next_run_at TIMESTAMP NOT NULL,
				updated_at TIMESTAMP NOT NULL,
				PRIMARY KEY (tenant_id, job_name)
			)`,
			`CREATE TABLE IF NOT EXISTS _kora_schedule_occurrence (
				tenant_id VARCHAR(140) NOT NULL,
				job_name VARCHAR(140) NOT NULL,
				scheduled_at TIMESTAMP NOT NULL,
				definition_digest CHAR(64) NOT NULL,
				job_json TEXT NOT NULL,
				state VARCHAR(24) NOT NULL,
				owner VARCHAR(140) NOT NULL DEFAULT '',
				fence BIGINT NOT NULL DEFAULT 0,
				attempts BIGINT NOT NULL DEFAULT 0,
				lease_until TIMESTAMP,
				finished_at TIMESTAMP,
				last_error TEXT NOT NULL,
				PRIMARY KEY (tenant_id, job_name, scheduled_at)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_schedule_due ON _kora_schedule_occurrence (tenant_id, job_name, state, scheduled_at)`,
			`CREATE INDEX IF NOT EXISTS idx_schedule_lease ON _kora_schedule_occurrence (tenant_id, job_name, state, lease_until)`,
			`CREATE INDEX IF NOT EXISTS idx_schedule_pending_backlog ON _kora_schedule_occurrence (tenant_id, state, scheduled_at)`,
		}
	case "libsql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_schedule_definition (
				tenant_id TEXT NOT NULL,
				job_name TEXT NOT NULL,
				definition_digest TEXT NOT NULL,
				schedule TEXT NOT NULL,
				job_json TEXT NOT NULL,
				next_run_at TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				PRIMARY KEY (tenant_id, job_name)
			)`,
			`CREATE TABLE IF NOT EXISTS _kora_schedule_occurrence (
				tenant_id TEXT NOT NULL,
				job_name TEXT NOT NULL,
				scheduled_at TEXT NOT NULL,
				definition_digest TEXT NOT NULL,
				job_json TEXT NOT NULL,
				state TEXT NOT NULL,
				owner TEXT NOT NULL DEFAULT '',
				fence INTEGER NOT NULL DEFAULT 0,
				attempts INTEGER NOT NULL DEFAULT 0,
				lease_until TEXT,
				finished_at TEXT,
				last_error TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (tenant_id, job_name, scheduled_at)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_schedule_due ON _kora_schedule_occurrence (tenant_id, job_name, state, scheduled_at)`,
			`CREATE INDEX IF NOT EXISTS idx_schedule_lease ON _kora_schedule_occurrence (tenant_id, job_name, state, lease_until)`,
			`CREATE INDEX IF NOT EXISTS idx_schedule_pending_backlog ON _kora_schedule_occurrence (tenant_id, state, scheduled_at)`,
		}
	default:
		panic(fmt.Sprintf("unsupported scheduler schema dialect %q", driver))
	}
}
