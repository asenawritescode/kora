package orm

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// linkUse is one Link value on a record or one of its child rows, kept so a
// missing target can be reported against the field that holds it.
type linkUse struct {
	owner string // the record type holding the Link: the record or a child row type
	field doctype.Field
	value string
}

// linkTargets groups the Link values a record holds by the record type they
// point at.
type linkTargets map[string][]linkUse

// validateLinkTargets refuses a record whose Link fields, or whose child rows'
// Link fields, name a record that does not exist. It runs inside the write's
// transaction, so a record written earlier in the same operation counts as
// existing. Empty values are not set; links to entity-owned records are left to
// validateEntityReferences, and links to types outside the registry (users,
// system tables) are not checked here.
func (tx *TxManager) validateLinkTargets(queryer db.Queryer, dt *doctype.DocType, doc *doctype.Document) error {
	if tx == nil || tx.Registry == nil || dt == nil || doc == nil {
		return nil
	}
	targets := linkTargets{}
	tx.collectLinks(targets, dt, doc)
	for _, f := range dt.TableFields() {
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			continue
		}
		for _, child := range doc.GetTable(f.Fieldname) {
			tx.collectLinks(targets, childDT, child)
		}
	}
	for targetName, uses := range targets {
		if err := tx.requireLinkTargets(queryer, tx.Registry.Get(targetName), uses); err != nil {
			return err
		}
	}
	return nil
}

// collectLinks adds the set Link values of doc to targets.
func (tx *TxManager) collectLinks(targets linkTargets, dt *doctype.DocType, doc *doctype.Document) {
	for _, field := range dt.Fields {
		if field.Fieldtype != "Link" || field.Fieldname == dt.BusinessScopeField {
			continue
		}
		target := tx.Registry.Get(field.Options)
		if target == nil || target.BusinessScopePolicy == contract.ScopeEntityOwned {
			continue
		}
		value := linkValue(doc.Get(field.Fieldname))
		if value == "" {
			continue
		}
		targets[target.Name] = append(targets[target.Name], linkUse{owner: dt.Name, field: field, value: value})
	}
}

// requireLinkTargets looks the distinct values up in one query and reports the
// first one that does not exist.
func (tx *TxManager) requireLinkTargets(queryer db.Queryer, target *doctype.DocType, uses []linkUse) error {
	names := distinctLinkValues(uses)
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	query := fmt.Sprintf("SELECT name FROM %s WHERE name IN (%s)", tx.tableName(target), placeholders)
	args := make([]any, len(names))
	for i, name := range names {
		args[i] = name
	}
	rows, err := queryer.Query(db.Rebind(tx.Dialect, query), args...)
	if err != nil {
		return fmt.Errorf("checking links to %s: %w", target.Name, err)
	}
	defer func() { _ = rows.Close() }() // read-only cursor; Err below reports read failures
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("checking links to %s: %w", target.Name, err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checking links to %s: %w", target.Name, err)
	}
	for _, use := range uses {
		if !found[use.value] {
			return fmt.Errorf("%w: %w", ErrValidation, &doctype.ValidationError{
				Type:    "LinkValidationError",
				Message: fmt.Sprintf("The %s you picked no longer exists. Choose another one.", strings.ToLower(use.field.Label)),
				Field:   use.field.Fieldname,
				DocType: use.owner,
			})
		}
	}
	return nil
}

// linkValue is a Link field's value as stored, or "" when it is not set.
func linkValue(raw any) string {
	if raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

// distinctLinkValues lists each linked name once, in first-seen order.
func distinctLinkValues(uses []linkUse) []string {
	seen := map[string]bool{}
	var names []string
	for _, use := range uses {
		if !seen[use.value] {
			seen[use.value] = true
			names = append(names, use.value)
		}
	}
	return names
}
