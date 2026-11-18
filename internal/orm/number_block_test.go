package orm

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestBlockStateMovesOnlyAlongAllowedEdges(t *testing.T) {
	states := []BlockState{blockNew, BlockOpen, BlockExhausted, BlockReleased, "bogus"}
	allowed := map[[2]BlockState]bool{
		{blockNew, BlockOpen}:           true,
		{BlockOpen, BlockExhausted}:     true,
		{BlockOpen, BlockReleased}:      true,
		{BlockExhausted, BlockReleased}: true,
	}
	for _, from := range states {
		for _, to := range states {
			want := allowed[[2]BlockState{from, to}]
			if got := from.CanMove(to); got != want {
				t.Errorf("CanMove(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	if err := transitionBlock(BlockReleased, BlockOpen); err == nil {
		t.Error("a released block reopened without error")
	}
}

func TestReserveNumberBlockRejectsInvalidRequestsBeforeTouchingTheDatabase(t *testing.T) {
	series := &doctype.DocType{Name: "Sale", NamingSeries: "SALE-.YYYY.-.#####"}
	cases := []struct {
		name  string
		dt    *doctype.DocType
		owner string
		size  int64
	}{
		{"no naming series", &doctype.DocType{Name: "Note"}, "till-1", 50},
		{"empty owner", series, "", 50},
		{"zero size", series, "till-1", 0},
		{"size above cap", series, "till-1", maxBlockSize + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := NumberBlockRequest{Owner: tc.owner, Size: tc.size, At: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
			if _, err := ReserveNumberBlock(nil, nil, tc.dt, req); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
