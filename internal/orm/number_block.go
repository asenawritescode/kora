package orm

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// maxBlockSize caps one reservation so one owner cannot hold a long series range.
const maxBlockSize = 1000

// BlockState is the lifecycle of a reserved range of naming-series numbers.
type BlockState string

const (
	// BlockOpen means the block still has numbers that may be used.
	BlockOpen BlockState = "open"
	// BlockExhausted means every number in the block is used.
	BlockExhausted BlockState = "exhausted"
	// BlockReleased means the block is closed. Its unused numbers are never reissued.
	BlockReleased BlockState = "released"
)

// blockNew is the state before a block row exists.
const blockNew BlockState = ""

// blockTransitions lists every allowed move. Anything else is a bug.
var blockTransitions = map[BlockState][]BlockState{
	blockNew:       {BlockOpen},
	BlockOpen:      {BlockExhausted, BlockReleased},
	BlockExhausted: {BlockReleased},
}

// CanMove reports whether a block may move from s to next.
func (s BlockState) CanMove(next BlockState) bool {
	return slices.Contains(blockTransitions[s], next)
}

// NumberBlock is a contiguous range of one naming series reserved for an owner.
// First and Last are inclusive counter values under Prefix.
type NumberBlock struct {
	Doctype   string
	Prefix    string
	Owner     string
	First     int64
	Last      int64
	FirstName string
	LastName  string
	State     BlockState
}

// NumberBlockRequest asks for one block. At selects the prefix: the series
// must not need field values, so a block never spans two prefixes.
type NumberBlockRequest struct {
	Owner string
	Size  int64
	At    time.Time
}

// ReserveNumberBlock takes the next req.Size numbers of dt's naming series for
// req.Owner and records the block as open.
func ReserveNumberBlock(dbTx *sql.Tx, dialect db.Dialect, dt *doctype.DocType, req NumberBlockRequest) (NumberBlock, error) {
	if dt.NamingSeries == "" {
		return NumberBlock{}, &doctype.ValidationError{Type: "NamingSeries", Message: "doctype has no naming series", DocType: dt.Name}
	}
	if req.Owner == "" {
		return NumberBlock{}, errors.New("number block owner is required")
	}
	if req.Size < 1 || req.Size > maxBlockSize {
		return NumberBlock{}, fmt.Errorf("number block size must be 1 to %d", maxBlockSize)
	}
	owner, size, at := req.Owner, req.Size, req.At
	series, err := doctype.ParseNamingSeries(dt.NamingSeries)
	if err != nil {
		return NumberBlock{}, err
	}
	prefix, err := series.Prefix(nil, at)
	if err != nil {
		return NumberBlock{}, &doctype.ValidationError{Type: "NamingSeries", Message: err.Error(), DocType: dt.Name}
	}
	first, last, err := dialect.ReserveSeriesRange(dbTx, dt.Name, seriesCounterKey(prefix), size, func() (int64, error) {
		return highestSeriesNumber(dbTx, dialect, dt.RawTableName(), prefix)
	})
	if err != nil {
		return NumberBlock{}, err
	}
	block := NumberBlock{
		Doctype: dt.Name, Prefix: prefix, Owner: owner, First: first, Last: last,
		FirstName: series.Format(prefix, first), LastName: series.Format(prefix, last),
	}
	if err := transitionBlock(blockNew, BlockOpen); err != nil {
		return NumberBlock{}, err
	}
	block.State = BlockOpen
	if err := dialect.RecordNumberBlock(dbTx, db.NumberBlockRow{Doctype: block.Doctype, Prefix: block.Prefix, Owner: block.Owner, State: string(block.State), First: block.First, Last: block.Last}); err != nil {
		return NumberBlock{}, err
	}
	return block, nil
}

// transitionBlock returns an error unless a block may move from one state to another.
func transitionBlock(from, to BlockState) error {
	if !from.CanMove(to) {
		return fmt.Errorf("number block cannot move from %q to %q", from, to)
	}
	return nil
}
