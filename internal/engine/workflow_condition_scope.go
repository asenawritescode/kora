package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/expression"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
)

// tenantWorkflowConditionScope reads one pinned subject through the normal
// tenant ORM and exposes only non-hidden scalar fields. It deliberately has
// no aggregate or arbitrary lookup path.
type tenantWorkflowConditionScope struct {
	runtime *knet.LoadedSite
	dialect db.Dialect
}

func newTenantWorkflowConditionScope(runtime *knet.LoadedSite, dialect db.Dialect) (*tenantWorkflowConditionScope, error) {
	if runtime == nil || runtime.DB == nil || runtime.Registry == nil || dialect == nil {
		return nil, fmt.Errorf("workflow condition scope requires tenant database, registry and dialect")
	}
	return &tenantWorkflowConditionScope{runtime: runtime, dialect: dialect}, nil
}

func (p *tenantWorkflowConditionScope) Scope(ctx context.Context, execution policy.Execution, instance contract.WorkflowInstance) (expression.Scope, error) {
	if p == nil || ctx == nil || p.runtime == nil || !execution.Valid() {
		return nil, contract.NewError(contract.CodeUnauthenticated, "workflow condition scope identity is unavailable")
	}
	tenant, err := contract.ParseTenantID(p.runtime.SiteID)
	if err != nil || execution.Tenant() != tenant || execution.Actor().Site != p.runtime.Name {
		return nil, contract.NewError(contract.CodePermissionDenied, "workflow condition scope tenant binding is invalid")
	}
	var grant policy.Grant
	var ok bool
	if instance.BusinessScope.Kind == contract.ScopeKindLegalEntity {
		grant, ok = execution.GrantForScope(instance.Subject.Kind, "read", instance.BusinessScope)
	} else {
		grant, ok = execution.GrantFor(instance.Subject.Kind, "read")
	}
	if !ok || grant.OwnerOnly {
		return nil, contract.NewError(contract.CodePermissionDenied, "workflow condition requires an unscoped subject read grant")
	}
	if _, err := contract.ParseRevision(string(instance.Subject.Revision)); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow condition subject revision is invalid")
	}
	if instance.Subject.ID == "" {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow condition subject identity is invalid")
	}
	dt := p.runtime.Registry.Get(instance.Subject.Kind)
	if dt == nil || dt.Name != instance.Subject.Kind {
		return nil, contract.NewError(contract.CodeNotFound, "workflow condition subject type is unavailable")
	}
	tx := &orm.TxManager{DB: p.runtime.DB, Registry: p.runtime.Registry, Dialect: p.dialect, Context: ctx, BusinessScope: instance.BusinessScope}
	doc, err := tx.GetDoc(dt, instance.Subject.ID, "")
	if err != nil {
		if errors.Is(err, orm.ErrNotFound) {
			return nil, contract.NewError(contract.CodeNotFound, "workflow condition subject is unavailable")
		}
		return nil, fmt.Errorf("load workflow condition subject: %w", err)
	}
	if strconv.FormatUint(doc.Revision, 10) != string(instance.Subject.Revision) {
		return nil, contract.NewError(contract.CodeStaleSubject, "workflow condition subject changed after workflow start")
	}
	values := make(map[string]expression.Value, len(dt.NonTableDataFields())+3)
	values["doc.name"] = expression.StringValue(doc.Name)
	values["doc.doc_status"] = expression.NumberValue(float64(doc.DocStatus))
	values["doc.revision"] = expression.NumberValue(float64(doc.Revision))
	for _, field := range dt.NonTableDataFields() {
		if field.Hidden || field.Fieldtype == "Password" {
			continue
		}
		if value, ok := workflowExpressionValue(field, doc.Get(field.Fieldname)); ok {
			values["doc."+field.Fieldname] = value
		}
	}
	return workflowConditionValues(values), nil
}

func workflowExpressionValue(field doctype.Field, raw any) (expression.Value, bool) {
	if raw == nil {
		return expression.NullValue(), true
	}
	switch field.Fieldtype {
	case "Check":
		switch value := raw.(type) {
		case bool:
			return expression.BoolValue(value), true
		case int64:
			return expression.BoolValue(value != 0), true
		case int:
			return expression.BoolValue(value != 0), true
		case string:
			return expression.BoolValue(value == "1" || value == "true"), true
		default:
			return expression.Value{}, false
		}
	case "Int", "Currency", "Float", "Percent":
		switch value := raw.(type) {
		case int:
			return expression.NumberValue(float64(value)), true
		case int64:
			return expression.NumberValue(float64(value)), true
		case json.Number:
			f, err := value.Float64()
			return expression.NumberValue(f), err == nil
		case float64:
			return expression.NumberValue(value), true
		case []byte:
			n, err := strconv.ParseFloat(string(value), 64)
			return expression.NumberValue(n), err == nil
		case string:
			n, err := strconv.ParseFloat(value, 64)
			return expression.NumberValue(n), err == nil
		default:
			return expression.Value{}, false
		}
	default:
		if value, ok := raw.(string); ok {
			return expression.StringValue(value), true
		}
		if value, ok := raw.([]byte); ok {
			return expression.StringValue(string(value)), true
		}
		return expression.Value{}, false
	}
}

type workflowConditionValues map[string]expression.Value

func (s workflowConditionValues) Lookup(path string) (expression.Value, bool) {
	value, ok := s[path]
	return value, ok
}

func (workflowConditionValues) Aggregate(string, string, string) (expression.Value, error) {
	return expression.NullValue(), contract.NewError(contract.CodePermissionDenied, "workflow condition aggregates are not available")
}
