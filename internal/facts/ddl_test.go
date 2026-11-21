package facts

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
)

func TestDDL(t *testing.T) {
	tests := []struct {
		dialect string
		want    []string
	}{
		{"mysql", []string{
			"CREATE TABLE IF NOT EXISTS `_kora_fact_sale` (\n  `record_id` VARCHAR(140) NOT NULL,\n  `line_id` VARCHAR(140) NOT NULL,\n  `legal_entity` VARCHAR(140) NOT NULL,\n  `owner` VARCHAR(140) NOT NULL,\n  `business_date` DATE NOT NULL,\n  `occurred_at` DATETIME(6) NOT NULL,\n  `status` VARCHAR(140) NOT NULL,\n  `source_sequence` BIGINT NOT NULL,\n  `deleted` TINYINT(1) NOT NULL,\n  `register` VARCHAR(140) NOT NULL,\n  `item` VARCHAR(140) NOT NULL,\n  `total` DECIMAL(21,9) NOT NULL,\n  `line_amount` DECIMAL(21,9) NOT NULL,\n  PRIMARY KEY (`record_id`, `line_id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
			"CREATE INDEX `idx__kora_fact_sale_business_date` ON `_kora_fact_sale` (`business_date`)",
			"CREATE INDEX `idx__kora_fact_sale_source_sequence` ON `_kora_fact_sale` (`source_sequence`)",
			"CREATE TABLE IF NOT EXISTS `_kora_fact_sale_line` (\n  `record_id` VARCHAR(140) NOT NULL,\n  `line_id` VARCHAR(140) NOT NULL,\n  `legal_entity` VARCHAR(140) NOT NULL,\n  `owner` VARCHAR(140) NOT NULL,\n  `business_date` DATE NOT NULL,\n  `occurred_at` DATETIME(6) NOT NULL,\n  `status` VARCHAR(140) NOT NULL,\n  `source_sequence` BIGINT NOT NULL,\n  `deleted` TINYINT(1) NOT NULL,\n  `register` VARCHAR(140) NOT NULL,\n  `item` VARCHAR(140) NOT NULL,\n  `total` DECIMAL(21,9) NOT NULL,\n  `line_amount` DECIMAL(21,9) NOT NULL,\n  PRIMARY KEY (`record_id`, `line_id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
			"CREATE INDEX `idx__kora_fact_sale_line_business_date` ON `_kora_fact_sale_line` (`business_date`)",
			"CREATE INDEX `idx__kora_fact_sale_line_source_sequence` ON `_kora_fact_sale_line` (`source_sequence`)",
		}},
		{"postgres", []string{
			"CREATE TABLE IF NOT EXISTS \"_kora_fact_sale\" (\n  \"record_id\" VARCHAR(140) NOT NULL,\n  \"line_id\" VARCHAR(140) NOT NULL,\n  \"legal_entity\" VARCHAR(140) NOT NULL,\n  \"owner\" VARCHAR(140) NOT NULL,\n  \"business_date\" DATE NOT NULL,\n  \"occurred_at\" TIMESTAMP NOT NULL,\n  \"status\" VARCHAR(140) NOT NULL,\n  \"source_sequence\" BIGINT NOT NULL,\n  \"deleted\" BOOLEAN NOT NULL,\n  \"register\" VARCHAR(140) NOT NULL,\n  \"item\" VARCHAR(140) NOT NULL,\n  \"total\" DECIMAL(21,9) NOT NULL,\n  \"line_amount\" DECIMAL(21,9) NOT NULL,\n  PRIMARY KEY (\"record_id\", \"line_id\")\n)",
			"CREATE INDEX IF NOT EXISTS \"idx__kora_fact_sale_business_date\" ON \"_kora_fact_sale\" (\"business_date\")",
			"CREATE INDEX IF NOT EXISTS \"idx__kora_fact_sale_source_sequence\" ON \"_kora_fact_sale\" (\"source_sequence\")",
			"CREATE TABLE IF NOT EXISTS \"_kora_fact_sale_line\" (\n  \"record_id\" VARCHAR(140) NOT NULL,\n  \"line_id\" VARCHAR(140) NOT NULL,\n  \"legal_entity\" VARCHAR(140) NOT NULL,\n  \"owner\" VARCHAR(140) NOT NULL,\n  \"business_date\" DATE NOT NULL,\n  \"occurred_at\" TIMESTAMP NOT NULL,\n  \"status\" VARCHAR(140) NOT NULL,\n  \"source_sequence\" BIGINT NOT NULL,\n  \"deleted\" BOOLEAN NOT NULL,\n  \"register\" VARCHAR(140) NOT NULL,\n  \"item\" VARCHAR(140) NOT NULL,\n  \"total\" DECIMAL(21,9) NOT NULL,\n  \"line_amount\" DECIMAL(21,9) NOT NULL,\n  PRIMARY KEY (\"record_id\", \"line_id\")\n)",
			"CREATE INDEX IF NOT EXISTS \"idx__kora_fact_sale_line_business_date\" ON \"_kora_fact_sale_line\" (\"business_date\")",
			"CREATE INDEX IF NOT EXISTS \"idx__kora_fact_sale_line_source_sequence\" ON \"_kora_fact_sale_line\" (\"source_sequence\")",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.dialect, func(t *testing.T) {
			got := DDL(db.Resolve(tc.dialect), saleFacts())
			if strings.Join(got, "\n;\n") != strings.Join(tc.want, "\n;\n") {
				t.Fatalf("DDL =\n%s\nwant\n%s", strings.Join(got, "\n;\n"), strings.Join(tc.want, "\n;\n"))
			}
		})
	}
}

func TestDDLWithoutFacts(t *testing.T) {
	dt := saleFacts()
	dt.Facts = nil
	if got := DDL(db.Resolve("mysql"), dt); got != nil {
		t.Fatalf("DDL without facts = %v, want nil", got)
	}
}
