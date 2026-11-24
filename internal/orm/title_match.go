package orm

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/textmatch"
)

// Caps of one match, and the kinds of a TitleMatches result in the order MatchTitles tries them.
const (
	maxTitleMatches = 50
	typoCandidates  = 500
	closestTitles   = 3
	TitleKindCode   = "code"
	TitleKindWords  = "words"
	TitleKindTypo   = "typo"
)

// TitleMatch is one record name with its display title.
type TitleMatch struct {
	Name  string
	Title string
}

// TitleMatches is the result of MatchTitles. Kind is empty when nothing matched; Closest then holds the
// nearest titles for a message.
type TitleMatches struct {
	Kind    string
	Matches []TitleMatch
	Closest []TitleMatch
}

// MatchTitles finds the records of dt that text names: the record with that exact name, else the records whose
// title holds every word of text, else those of the last typoCandidates by name (primary-key order, no table
// sort) whose title is a typo away from every word. Rows the scope does not allow are never matched.
func (tx *TxManager) MatchTitles(ctx context.Context, dt *doctype.DocType, text string, scope policy.RowScope) (TitleMatches, error) {
	text = strings.TrimSpace(text)
	words := textmatch.Words(text)
	if len(words) == 0 {
		return TitleMatches{}, nil
	}
	title := dt.TitleField
	if title == "" {
		title = "name"
	}
	q := tx.Dialect.QuoteIdent
	code, err := tx.queryTitles(ctx, dt, scope, titleQuery{title: title, where: []string{q("name") + " = ?"}, args: []any{text}, order: q("name"), limit: 1})
	if err != nil {
		return TitleMatches{}, err
	}
	if len(code) > 0 {
		return TitleMatches{Kind: TitleKindCode, Matches: code}, nil
	}
	query := titleQuery{title: title, order: q(title), limit: maxTitleMatches}
	for _, word := range words {
		query.where = append(query.where, "LOWER("+q(title)+") LIKE ? ESCAPE '!'")
		query.args = append(query.args, "%"+textmatch.LikeEscape(word)+"%")
	}
	rows, err := tx.queryTitles(ctx, dt, scope, query)
	if err != nil {
		return TitleMatches{}, err
	}
	if len(rows) > 0 {
		return TitleMatches{Kind: TitleKindWords, Matches: rows}, nil
	}
	candidates, err := tx.queryTitles(ctx, dt, scope, titleQuery{title: title, order: q("name") + " DESC", limit: typoCandidates})
	if err != nil {
		return TitleMatches{}, err
	}
	return typoTitleMatches(candidates, words), nil
}

// typoTitleMatches keeps the candidates whose title is a typo away from every word, or else the closest titles.
func typoTitleMatches(candidates []TitleMatch, words []string) TitleMatches {
	type ranked struct {
		match TitleMatch
		dist  int
	}
	var out TitleMatches
	all := make([]ranked, 0, len(candidates))
	for _, c := range candidates {
		ok, dist := textmatch.Fuzzy(words, textmatch.Words(c.Title))
		if ok && len(out.Matches) < maxTitleMatches {
			out.Matches = append(out.Matches, c)
		}
		all = append(all, ranked{match: c, dist: dist})
	}
	if len(out.Matches) > 0 {
		out.Kind = TitleKindTypo
		return out
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].dist < all[j].dist })
	for i := 0; i < len(all) && i < closestTitles; i++ {
		out.Closest = append(out.Closest, all[i].match)
	}
	return out
}

// titleQuery is the WHERE, ORDER BY and LIMIT of one title read. Where and args align; the scope is added last.
type titleQuery struct {
	title string
	where []string
	args  []any
	order string
	limit int
}

// queryTitles reads name and title of the records of dt that match the query and the row scope.
func (tx *TxManager) queryTitles(ctx context.Context, dt *doctype.DocType, scope policy.RowScope, query titleQuery) ([]TitleMatch, error) {
	scoped, scopedArgs, err := tx.rowScopePredicate(dt, scope)
	if err != nil {
		return nil, err
	}
	where, args := query.where, query.args
	if scoped != "" {
		where = append(where, "("+scoped+")")
		args = append(args, scopedArgs...)
	}
	clause := strings.Join(append(where, "1=1"), " AND ")
	q := tx.Dialect.QuoteIdent
	statement := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s ORDER BY %s LIMIT %d",
		q("name"), q(query.title), tx.tableName(dt), clause, query.order, query.limit)
	rows, err := tx.queryer().QueryContext(ctx, db.Rebind(tx.Dialect, statement), args...)
	if err != nil {
		return nil, fmt.Errorf("matching titles of %s: %w", dt.Name, err)
	}
	defer rows.Close()
	var out []TitleMatch
	for rows.Next() {
		var m TitleMatch
		var title sql.NullString
		if err := rows.Scan(&m.Name, &title); err != nil {
			return nil, fmt.Errorf("reading title of %s: %w", dt.Name, err)
		}
		m.Title = title.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountUpTo counts the records of dt the scope allows, reading at most limit of them. It serves a total in a
// message, so a large table is never counted whole.
func (tx *TxManager) CountUpTo(ctx context.Context, dt *doctype.DocType, scope policy.RowScope, limit int) (int, error) {
	scoped, args, err := tx.rowScopePredicate(dt, scope)
	if err != nil {
		return 0, err
	}
	where := "1=1"
	if scoped != "" {
		where = "(" + scoped + ")"
	}
	statement := fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE %s LIMIT ?) c", tx.tableName(dt), where)
	var total int
	if err := tx.queryer().QueryRowContext(ctx, db.Rebind(tx.Dialect, statement), append(args, limit)...).Scan(&total); err != nil {
		return 0, fmt.Errorf("counting %s: %w", dt.Name, err)
	}
	return total, nil
}
