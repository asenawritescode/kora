package analytics

import (
	"fmt"
	"strings"
	"testing"
)

// Benchmark batch INSERT query construction — the core optimization in flush().

func BenchmarkBuildBatchInsert_N100(b *testing.B) {
	deltas := make(map[deltaKey]float64, 100)
	for i := 0; i < 100; i++ {
		deltas[deltaKey{
			Doctype:   "Customer",
			Metric:    "customer_count",
			Dimension: fmt.Sprintf("status=active_%d", i%5),
			Date:      "2026-06-24",
		}] = float64(i + 1)
	}
	b.ResetTimer()

	for b.Loop() {
		const rowCols = 6
		n := len(deltas)
		rowPlaceholder := "(" + strings.Repeat("?,", rowCols-1) + "?)"
		placeholders := strings.Repeat(rowPlaceholder+",", n-1) + rowPlaceholder

		args := make([]any, 0, n*rowCols)
		for key, delta := range deltas {
			args = append(args, "site1", key.Doctype, key.Metric, key.Dimension, key.Date, delta)
		}

		_ = "INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) VALUES " +
			placeholders + " ON DUPLICATE KEY UPDATE value = value + VALUES(value)"
		_ = args
	}
}

func BenchmarkBuildBatchInsert_N500(b *testing.B) {
	deltas := make(map[deltaKey]float64, 500)
	for i := 0; i < 500; i++ {
		deltas[deltaKey{
			Doctype:   "Customer",
			Metric:    "customer_count",
			Dimension: fmt.Sprintf("status=active_%d", i%10),
			Date:      "2026-06-24",
		}] = float64(i + 1)
	}
	b.ResetTimer()

	for b.Loop() {
		const rowCols = 6
		n := len(deltas)
		rowPlaceholder := "(" + strings.Repeat("?,", rowCols-1) + "?)"
		placeholders := strings.Repeat(rowPlaceholder+",", n-1) + rowPlaceholder

		args := make([]any, 0, n*rowCols)
		for key, delta := range deltas {
			args = append(args, "site1", key.Doctype, key.Metric, key.Dimension, key.Date, delta)
		}

		_ = "INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) VALUES " +
			placeholders + " ON DUPLICATE KEY UPDATE value = value + VALUES(value)"
		_ = args
	}
}

// Benchmark anyToString vs fmt.Sprintf for the hot path dimension building.
func BenchmarkAnyToString_String(b *testing.B) {
	v := "Active"
	for b.Loop() {
		_ = anyToString(v)
	}
}

func BenchmarkSprintf_String(b *testing.B) {
	v := "Active"
	for b.Loop() {
		_ = fmt.Sprintf("%v", v)
	}
}

func BenchmarkAnyToString_Int(b *testing.B) {
	v := 42
	for b.Loop() {
		_ = anyToString(v)
	}
}

func BenchmarkSprintf_Int(b *testing.B) {
	v := 42
	for b.Loop() {
		_ = fmt.Sprintf("%v", v)
	}
}

func BenchmarkStringConcat(b *testing.B) {
	field := "status"
	val := "Active"
	for b.Loop() {
		_ = field + "=" + val
	}
}

func BenchmarkSprintf_Dimension(b *testing.B) {
	field := "status"
	val := "Active"
	for b.Loop() {
		_ = fmt.Sprintf("%s=%v", field, val)
	}
}
