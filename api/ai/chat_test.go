package ai

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
)

func TestConversationSubjectKeySeparatesExplicitThreads(t *testing.T) {
	context := ChatContext{Pathname: "/workspace", ShellMode: "workspace"}
	legacy := ConversationSubjectKey("alice", context)
	first := ConversationSubjectKey("alice", ChatContext{Pathname: context.Pathname, ShellMode: context.ShellMode, ThreadID: "thread-one"})
	second := ConversationSubjectKey("alice", ChatContext{Pathname: context.Pathname, ShellMode: context.ShellMode, ThreadID: "thread-two"})
	otherUser := ConversationSubjectKey("bob", ChatContext{Pathname: context.Pathname, ShellMode: context.ShellMode, ThreadID: "thread-one"})

	if legacy != "alice:/workspace::" {
		t.Fatalf("legacy thread key changed: %q", legacy)
	}
	if first == second || first == legacy || first == otherUser {
		t.Fatalf("thread or user histories are not isolated: legacy=%q first=%q second=%q other=%q", legacy, first, second, otherUser)
	}
	if len(first) > 80 {
		t.Fatalf("thread key is not bounded for the database column: length=%d", len(first))
	}
}

func TestSiteCurrencyForChatUsesTenantSetting(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	query := "SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "currency").WillReturnRows(
		sqlmock.NewRows([]string{"setting_value"}).AddRow("UGX"),
	)
	currency, err := siteCurrencyForChat(context.Background(), database, "tenant-a", kdb.Resolve("sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if currency != "UGX" {
		t.Fatalf("site currency = %q, want UGX", currency)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSiteCurrencyForChatUsesSystemDefaultWhenUnset(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	query := "SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "currency").WillReturnRows(sqlmock.NewRows([]string{"setting_value"}))
	currency, err := siteCurrencyForChat(context.Background(), database, "tenant-a", kdb.Resolve("sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if currency != "KES" {
		t.Fatalf("unset site's currency = %q, want system default KES", currency)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFinancialDisplayContextUsesResolvedCurrency(t *testing.T) {
	context := financialDisplayContext("  usd ")
	if !strings.Contains(context, "configured currency is USD") || !strings.Contains(context, "Use USD") {
		t.Fatalf("currency instructions do not use normalized site currency: %q", context)
	}
	if strings.Contains(context, "KES") {
		t.Fatalf("currency instructions contain a different hardcoded currency: %q", context)
	}
}

func TestSiteCurrencyForChatUsesPostgresPlaceholders(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	query := "SELECT setting_value FROM _kora_site_setting WHERE site = $1 AND setting_key = $2"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "currency").WillReturnRows(
		sqlmock.NewRows([]string{"setting_value"}).AddRow(" tzs "),
	)
	currency, err := siteCurrencyForChat(context.Background(), database, "tenant-a", kdb.Resolve("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if currency != "TZS" {
		t.Fatalf("site currency = %q, want TZS", currency)
	}
}
