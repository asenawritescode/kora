package auth

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
)

func TestHashExtensionAccessToken(t *testing.T) {
	got := HashExtensionAccessToken("abc")
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("HashExtensionAccessToken(abc) = %s, want %s", got, want)
	}
	if HashExtensionAccessToken("abd") == got {
		t.Fatal("distinct tokens must not share a verifier")
	}
}

func TestBackfillExtensionTokenHashes(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery("SELECT site, name, access_token FROM _kora_extension WHERE access_token <> '' AND access_token_hash = ''").
		WillReturnRows(sqlmock.NewRows([]string{"site", "name", "access_token"}).AddRow("site-a", "bot", "legacy-token"))
	mock.ExpectExec("UPDATE _kora_extension SET access_token_hash = \\? WHERE site = \\? AND name = \\? AND access_token = \\?").
		WithArgs(HashExtensionAccessToken("legacy-token"), "site-a", "bot", "legacy-token").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := BackfillExtensionTokenHashes(context.Background(), database, kdb.Resolve("mysql")); err != nil {
		t.Fatalf("BackfillExtensionTokenHashes: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
