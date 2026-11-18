package kernel

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// createStepDocument builds the document a create step inserts. A create step
// with Name presets the record name, after the number check in presetRecordName.
func (k *Kernel) createStepDocument(dbTx *sql.Tx, txMgr *orm.TxManager, dt *doctype.DocType, create *StepCreate, at stepEnv) (*doctype.Document, *contract.Error) {
	doc, cerr := createDocumentValues(dt, create.Values, at.env, at.index)
	if cerr != nil || create.Name == "" {
		return doc, cerr
	}
	name, cerr := k.presetRecordName(dbTx, txMgr, dt, create, at.env)
	if cerr != nil {
		return nil, cerr
	}
	doc.Name = name
	return doc, nil
}

// presetRecordName resolves a create step's preset name. The name must be a
// number inside an open number block that the step's Block owner reserved, and
// no record may use it yet.
func (k *Kernel) presetRecordName(dbTx *sql.Tx, txMgr *orm.TxManager, dt *doctype.DocType, create *StepCreate, env refEnv) (string, *contract.Error) {
	name, nameErr := resolveEnvStringRef(create.Name, env)
	owner, ownerErr := resolveEnvStringRef(create.Block, env)
	if nameErr != nil || ownerErr != nil || name == "" || owner == "" {
		return "", contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s name and block owner are required", dt.Name))
	}
	series, err := doctype.ParseNamingSeries(dt.NamingSeries)
	if err != nil {
		return "", contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s has no usable naming series", dt.Name))
	}
	blocks, err := k.dialect().OpenNumberBlocks(dbTx, dt.Name, owner)
	if err != nil {
		return "", contract.NewError(contract.CodeDependencyUnavailable, "reading number blocks failed")
	}
	if !numberInOpenBlock(series, blocks, name) {
		return "", contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s %q is not a number reserved for %s", dt.Name, name, owner))
	}
	filters, _ := json.Marshal([][]any{{"name", "=", name}})
	used, err := txMgr.MatchingNamesLocked(dt, string(filters), policy.RowScope{}, 1)
	if err != nil {
		return "", contract.NewError(contract.CodeDependencyUnavailable, "checking the number failed")
	}
	if len(used) > 0 {
		return "", contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s %q is already used", dt.Name, name))
	}
	return name, nil
}

// numberInOpenBlock reports whether name is a number of series inside one of blocks.
func numberInOpenBlock(series doctype.NamingSeries, blocks []db.NumberBlockRow, name string) bool {
	for _, b := range blocks {
		n, ok := doctype.CounterFromName(b.Prefix, name)
		if ok && n >= b.First && n <= b.Last && series.Format(b.Prefix, n) == name {
			return true
		}
	}
	return false
}
