package outbox

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/storage"
)

type fakeArchiveStore struct{}

func (fakeArchiveStore) Put(context.Context, string, io.Reader, int64, storage.FileMeta) error {
	return nil
}

func TestArchiveKeyFormat(t *testing.T) {
	created := time.Date(2025, time.March, 9, 23, 0, 0, 0, time.FixedZone("EAT", 3*3600))
	rows := []archiveRow{{"id": "01FIRST"}, {"id": "01LAST"}}
	tests := []struct {
		name string
		site string
		when time.Time
		want string
	}{
		{"utc month from local zone", "acme", created, "outbox-archive/acme/2025/03/01FIRST_01LAST.ndjson.gz"},
		{"month boundary", "acme", time.Date(2025, time.December, 31, 22, 0, 0, 0, time.UTC), "outbox-archive/acme/2025/12/01FIRST_01LAST.ndjson.gz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := archiveKey(tt.site, tt.when, rows); got != tt.want {
				t.Fatalf("archiveKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewArchiverValidation(t *testing.T) {
	ok := Archiver{DB: new(sql.DB), Dialect: db.Resolve("mysql"), Store: fakeArchiveStore{}}
	tests := []struct {
		name        string
		mutate      func(a *Archiver)
		wantErr     error
		wantHorizon time.Duration
		wantChunk   int
	}{
		{"zero horizon and chunk take defaults", func(a *Archiver) {}, nil, DefaultArchiveHorizon, DefaultArchiveChunkSize},
		{"89 days is below the rebuild window", func(a *Archiver) { a.Horizon = 89 * 24 * time.Hour }, ErrArchiveHorizon, 0, 0},
		{"exactly 90 days is accepted", func(a *Archiver) { a.Horizon = MinArchiveHorizon }, nil, MinArchiveHorizon, DefaultArchiveChunkSize},
		{"missing store is rejected", func(a *Archiver) { a.Store = nil }, errArchiveDependencies, 0, 0},
		{"missing database is rejected", func(a *Archiver) { a.DB = nil }, errArchiveDependencies, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := ok
			tt.mutate(&a)
			got, err := NewArchiver(a)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Horizon != tt.wantHorizon || got.ChunkSize != tt.wantChunk {
				t.Fatalf("horizon %v chunk %d, want %v %d", got.Horizon, got.ChunkSize, tt.wantHorizon, tt.wantChunk)
			}
		})
	}
}

func TestNormalizeArchiveValue(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.FixedZone("X", 3600))
	tests := []struct {
		name string
		in   any
		want any
	}{
		{"bytes become text", []byte(`{"a":1}`), `{"a":1}`},
		{"instant becomes UTC RFC 3339", when, "2026-01-02T02:04:05.6Z"},
		{"nil stays nil", nil, nil},
		{"int stays int", int64(7), int64(7)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeArchiveValue(tt.in); got != tt.want {
				t.Fatalf("normalize = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEncodeArchiveWritesGzipNDJSON(t *testing.T) {
	rows := []archiveRow{{"id": "01A", "payload": `{"n":1}`}, {"id": "01B", "payload": nil}}
	body, err := encodeArchive(rows)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(readAll(t, zr)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d NDJSON lines, want 2", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first["id"] != "01A" {
		t.Fatalf("first line = %q (err %v)", lines[0], err)
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
