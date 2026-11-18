package kernel

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

// execReserveStep reserves the block a reserve step asks for and returns its
// outcome; the step's As name, if any, refers to the block's first record.
func (k *Kernel) execReserveStep(dbTx *sql.Tx, reg *doctype.Registry, step CommandStep, env refEnv, stepNames map[string]string) (stepOutcome, *contract.Error) {
	req := step.Reserve
	dt := reg.Get(req.Record)
	if dt == nil {
		return stepOutcome{}, contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", req.Record))
	}
	raw, err := resolveEnvRef(req.Owner, env)
	owner, isString := raw.(string)
	if err != nil || !isString || owner == "" {
		return stepOutcome{}, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("reserve owner %q must resolve to a name", req.Owner))
	}
	block, err := orm.ReserveNumberBlock(dbTx, k.dialect(), dt, orm.NumberBlockRequest{Owner: owner, Size: req.Size, At: time.Now().UTC()})
	if err != nil {
		return stepOutcome{}, wrapORMError(err, k.dialect())
	}
	if step.As != "" {
		stepNames[step.As] = block.FirstName
	}
	return stepOutcome{Record: req.Record, Name: block.FirstName, Created: true}, nil
}
