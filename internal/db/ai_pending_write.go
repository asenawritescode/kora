package db

// aiPendingWriteSQL is the table that holds a chat write from its preview until it is
// confirmed. A row is keyed by the hash of its confirm token, so the token itself is
// never stored. Its state moves previewed -> executing -> done | failed.
func (d *MySQLDialect) aiPendingWriteSQL() []string {
	return []string{
		"CREATE TABLE IF NOT EXISTS _kora_ai_pending_write (\n\t\t\tid VARCHAR(64) PRIMARY KEY,\n\t\t\tsite VARCHAR(140) NOT NULL,\n\t\t\tconversation_id VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\trun_id VARCHAR(255) NOT NULL,\n\t\t\tactor_id VARCHAR(255) NOT NULL,\n\t\t\ttool_name VARCHAR(140) NOT NULL,\n\t\t\targs_hash VARCHAR(64) NOT NULL,\n\t\t\targs_json LONGTEXT NOT NULL,\n\t\t\tpreview_json LONGTEXT NOT NULL,\n\t\t\tstate VARCHAR(20) NOT NULL,\n\t\t\tattempts INT NOT NULL DEFAULT 0,\n\t\t\tcreated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),\n\t\t\tclaimed_at DATETIME(6) NULL,\n\t\t\tcompleted_at DATETIME(6) NULL,\n\t\t\toutcome LONGTEXT NULL,\n\t\t\terror_message VARCHAR(255) NOT NULL DEFAULT '',\n\t\t\tINDEX idx_ai_pending_site_state (site, state)\n\t\t) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
	}
}
