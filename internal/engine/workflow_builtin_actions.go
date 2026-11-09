package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/email"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/workflow"
)

var errUnrecognizedWorkflowSchema = errors.New("workflow schema is not a built-in schema")

var (
	emailWorkflowInputSchema         = contract.SchemaRef{ID: "kora.email.send", Version: 1}
	emailWorkflowOutputSchema        = contract.SchemaRef{ID: "kora.email.send.result", Version: 1}
	statusWorkflowInputSchema        = contract.SchemaRef{ID: "kora.record.update_status", Version: 1}
	statusWorkflowOutputSchema       = contract.SchemaRef{ID: "kora.record.update_status.result", Version: 1}
	notificationWorkflowInputSchema  = contract.SchemaRef{ID: "kora.notification.send", Version: 1}
	notificationWorkflowOutputSchema = contract.SchemaRef{ID: "kora.notification.send.result", Version: 1}
)

func builtinWorkflowSchema(ref contract.SchemaRef) bool {
	return ref == emailWorkflowInputSchema || ref == emailWorkflowOutputSchema || ref == statusWorkflowInputSchema || ref == statusWorkflowOutputSchema || ref == notificationWorkflowInputSchema || ref == notificationWorkflowOutputSchema
}

func isWorkflowEmailField(field *doctype.Field) bool {
	return field != nil && (field.Fieldtype == "Email" || (strings.EqualFold(field.Fieldtype, "Data") && strings.EqualFold(strings.TrimSpace(field.Options), "Email")))
}

func validateBuiltinWorkflowSchema(ref contract.SchemaRef, raw json.RawMessage) error {
	if !builtinWorkflowSchema(ref) {
		return errUnrecognizedWorkflowSchema
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return fmt.Errorf("workflow action input must be an object")
	}
	if ref == emailWorkflowOutputSchema {
		var sent bool
		if len(value) != 1 || json.Unmarshal(value["sent"], &sent) != nil {
			return fmt.Errorf("email result must contain a sent boolean")
		}
		return nil
	}
	if ref == statusWorkflowOutputSchema {
		var result struct {
			Doctype   string         `json:"doctype"`
			Name      string         `json:"name"`
			Document  map[string]any `json:"document"`
			Operation string         `json:"operation_id"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Doctype == "" || result.Name == "" || result.Document == nil || result.Operation == "" {
			return fmt.Errorf("status result must identify the updated record")
		}
		return nil
	}
	if ref == notificationWorkflowOutputSchema {
		var result struct {
			Delivered int `json:"delivered"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Delivered < 1 || len(value) != 1 {
			return fmt.Errorf("notification result must contain a positive delivered count")
		}
		return nil
	}
	if ref == notificationWorkflowInputSchema {
		var config struct {
			Channel         string   `json:"channel"`
			RecipientFields []string `json:"recipient_fields"`
			StateField      string   `json:"state_field"`
			Subject         string   `json:"subject"`
			Message         string   `json:"message"`
		}
		if json.Unmarshal(raw, &config) != nil || (config.Channel != "in_app" && config.Channel != "email" && config.Channel != "both") || len(config.RecipientFields) == 0 || len(config.RecipientFields) > 50 || len(config.Subject) > 200 || len(config.Message) > 8192 || strings.TrimSpace(config.StateField) == "" {
			return fmt.Errorf("notification channel, recipients, subject, or message is invalid")
		}
		for _, recipient := range config.RecipientFields {
			if strings.TrimSpace(recipient) == "" || strings.TrimSpace(recipient) != recipient {
				return fmt.Errorf("notification recipient field is invalid")
			}
		}
		for key := range value {
			if key != "channel" && key != "recipient_fields" && key != "subject" && key != "message" && key != "state_field" {
				return fmt.Errorf("unsupported notification input field %q", key)
			}
		}
		return nil
	}
	if ref == statusWorkflowInputSchema {
		var config workflow.StatusActionConfig
		if json.Unmarshal(raw, &config) != nil || strings.TrimSpace(config.StateField) == "" || strings.TrimSpace(config.StateField) != config.StateField || len(config.StateField) > 160 || strings.TrimSpace(config.InitialState) == "" || strings.TrimSpace(config.InitialState) != config.InitialState || len(config.InitialState) > 160 {
			return fmt.Errorf("choose a status field and its initial value")
		}
		if issues := workflow.ValidateStatusTransition(config.StatusTransition); len(issues) > 0 {
			return fmt.Errorf("%s", issues[0].Detail)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return fmt.Errorf("status action input must be an object")
		}
		for key := range fields {
			if key != "state_field" && key != "initial_state" && key != "transition_action" && key != "from" && key != "to" && key != "allowed_roles" && key != "required_fields" && key != "condition" && key != "doc_status" {
				return fmt.Errorf("unsupported status action input field %q", key)
			}
		}
		return nil
	}
	var recipientField, subject, message string
	if json.Unmarshal(value["recipient_field"], &recipientField) != nil || strings.TrimSpace(recipientField) == "" {
		return fmt.Errorf("choose an email field for the recipient")
	}
	if json.Unmarshal(value["subject"], &subject) != nil || strings.TrimSpace(subject) == "" || len(subject) > 200 {
		return fmt.Errorf("email subject is required and must be at most 200 characters")
	}
	if json.Unmarshal(value["message"], &message) != nil || strings.TrimSpace(message) == "" || len(message) > 8192 {
		return fmt.Errorf("email message is required and must be at most 8 KiB")
	}
	for key := range value {
		if key != "recipient_field" && key != "subject" && key != "message" {
			return fmt.Errorf("unsupported email action input field %q", key)
		}
	}
	return nil
}

func notificationWorkflowRegistration(sender *email.Sender, database *sql.DB, registry *doctype.Registry, dialect db.Dialect, bus eventstream.Bus) workflow.ActionRegistration {
	return workflow.ActionRegistration{
		Key: workflow.ActionKey{Name: "notification.send", Version: 1}, Class: workflow.ActionExternal,
		InputSchema: notificationWorkflowInputSchema, OutputSchema: notificationWorkflowOutputSchema,
		RequiredGrant: policy.Grant{Resource: "workflow.notification", Operation: "submit"},
		External:      recordNotificationWorkflowAction{sender: sender, database: database, registry: registry, dialect: dialect, bus: bus},
	}
}

type recordNotificationWorkflowAction struct {
	sender   *email.Sender
	database *sql.DB
	registry *doctype.Registry
	dialect  db.Dialect
	bus      eventstream.Bus
}

func (a recordNotificationWorkflowAction) Execute(ctx context.Context, action workflow.ActionContext, input workflow.ActionInput) (workflow.ActionResult, error) {
	if ctx == nil || a.database == nil || a.registry == nil || a.dialect == nil || action.Subject.ID == "" {
		return workflow.ActionResult{}, fmt.Errorf("notification action is missing its tenant record context")
	}
	if err := validateBuiltinWorkflowSchema(notificationWorkflowInputSchema, input.Data); err != nil {
		return workflow.ActionResult{}, err
	}
	var config struct {
		Channel         string   `json:"channel"`
		RecipientFields []string `json:"recipient_fields"`
		StateField      string   `json:"state_field"`
		Subject         string   `json:"subject"`
		Message         string   `json:"message"`
	}
	if err := json.Unmarshal(input.Data, &config); err != nil {
		return workflow.ActionResult{}, err
	}
	dt := a.registry.Get(action.Subject.Kind)
	if dt == nil || dt.Name != action.Subject.Kind || dt.IsChildTable {
		return workflow.ActionResult{}, contract.NewError(contract.CodeValidationFailed, "notification subject is not a supported table record")
	}
	for _, name := range config.RecipientFields {
		field := dt.GetField(name)
		if field == nil || ((config.Channel == "email" || config.Channel == "both") && !isWorkflowEmailField(field)) {
			return workflow.ActionResult{}, contract.NewError(contract.CodeValidationFailed, "notification recipient must be an available Email field for email delivery")
		}
	}
	rows, err := a.database.QueryContext(ctx, db.Rebind(a.dialect, "SELECT * FROM "+a.dialect.QuoteIdent(dt.RawTableName())+" WHERE "+a.dialect.QuoteIdent("name")+" = ?"), action.Subject.ID)
	if err != nil {
		return workflow.ActionResult{}, fmt.Errorf("load notification record: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return workflow.ActionResult{}, fmt.Errorf("read notification record columns: %w", err)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return workflow.ActionResult{}, err
		}
		return workflow.ActionResult{}, contract.NewError(contract.CodeNotFound, "notification record was not found")
	}
	values := make([]any, len(columns))
	destinations := make([]any, len(columns))
	for i := range values {
		destinations[i] = &values[i]
	}
	if err := rows.Scan(destinations...); err != nil {
		return workflow.ActionResult{}, fmt.Errorf("scan notification record: %w", err)
	}
	data := map[string]string{"name": action.Subject.ID, "doctype": dt.Name}
	for index, column := range columns {
		value := values[index]
		if raw, ok := value.([]byte); ok {
			value = string(raw)
		}
		if value != nil {
			data[column] = fmt.Sprint(value)
		}
	}
	state := data[config.StateField]
	data["state"] = state
	subject := strings.TrimSpace(config.Subject)
	if subject == "" {
		subject = fmt.Sprintf("%s %s updated", dt.Name, action.Subject.ID)
	}
	message := strings.TrimSpace(config.Message)
	if message == "" {
		message = fmt.Sprintf("%s %s moved to %s", dt.Name, action.Subject.ID, state)
	}
	delivered := 0
	for _, field := range config.RecipientFields {
		recipient := strings.TrimSpace(data[field])
		if recipient == "" {
			continue
		}
		if config.Channel == "in_app" || config.Channel == "both" {
			if a.bus == nil {
				return workflow.ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "in-app notification service is unavailable")
			}
			if err := a.bus.Publish(eventstream.ChangeEvent{Site: action.Execution.Actor().Site, Doctype: dt.Name, DocName: action.Subject.ID, Operation: eventstream.EventNotification, Timestamp: time.Now().UTC(), ModifiedBy: action.Execution.Actor().PrincipalID, Data: map[string]any{"recipient": recipient, "title": subject, "message": message, "state": state, "source": "workflow"}}); err != nil {
				return workflow.ActionResult{}, fmt.Errorf("publish in-app workflow notification: %w", err)
			}
			delivered++
		}
		if config.Channel == "email" || config.Channel == "both" {
			address, err := mail.ParseAddress(recipient)
			if err != nil || address.Address != recipient {
				return workflow.ActionResult{}, contract.NewError(contract.CodeValidationFailed, "workflow notification Email field is not a valid address")
			}
			if a.sender == nil {
				return workflow.ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "email delivery service is unavailable")
			}
			if err := a.sender.SendTemplateContext(ctx, []string{recipient}, subject, message, data); err != nil {
				return workflow.ActionResult{}, err
			}
			delivered++
		}
	}
	if delivered == 0 {
		return workflow.ActionResult{}, contract.NewError(contract.CodeValidationFailed, "notification record has no recipient values")
	}
	result, _ := json.Marshal(map[string]int{"delivered": delivered})
	return workflow.ActionResult{Schema: notificationWorkflowOutputSchema, Data: result}, nil
}

func statusWorkflowRegistration() workflow.ActionRegistration {
	return workflow.ActionRegistration{Key: workflow.ActionKey{Name: "record.update_status", Version: 1}, Class: workflow.ActionLocal,
		InputSchema: statusWorkflowInputSchema, OutputSchema: statusWorkflowOutputSchema,
		RequiredGrant: policy.Grant{Resource: "workflow.status", Operation: "submit"}}
}

func statusWorkflowOperation(_ context.Context, action workflow.ActionContext, input workflow.ActionInput) (kernel.Operation, error) {
	if err := validateBuiltinWorkflowSchema(statusWorkflowInputSchema, input.Data); err != nil {
		return kernel.Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	var config struct {
		TransitionAction string `json:"transition_action"`
	}
	if err := json.Unmarshal(input.Data, &config); err != nil {
		return kernel.Operation{}, err
	}
	revision, err := strconv.ParseUint(string(action.Subject.Revision), 10, 64)
	if err != nil || revision == 0 {
		return kernel.Operation{}, contract.NewError(contract.CodeConflict, "record revision is unavailable for a status transition")
	}
	payload, err := json.Marshal(kernel.WorkflowStatusTransitionPayload{Doctype: action.Subject.Kind, Name: action.Subject.ID, Action: config.TransitionAction, ExpectedRevision: revision})
	if err != nil {
		return kernel.Operation{}, contract.NewError(contract.CodeInternal, "could not encode status transition")
	}
	return kernel.Operation{Command: kernel.CommandWorkflowStatusTransition, Payload: payload}, nil
}

func emailWorkflowRegistration(sender *email.Sender, database *sql.DB, registry *doctype.Registry, dialect db.Dialect) workflow.ActionRegistration {
	return workflow.ActionRegistration{
		Key: workflow.ActionKey{Name: "email.send", Version: 1}, Class: workflow.ActionExternal,
		InputSchema: emailWorkflowInputSchema, OutputSchema: emailWorkflowOutputSchema,
		RequiredGrant: policy.Grant{Resource: "workflow.email", Operation: "submit"},
		External:      recordEmailWorkflowAction{sender: sender, database: database, registry: registry, dialect: dialect},
	}
}

type recordEmailWorkflowAction struct {
	sender   *email.Sender
	database *sql.DB
	registry *doctype.Registry
	dialect  db.Dialect
}

func (a recordEmailWorkflowAction) Execute(ctx context.Context, actionContext workflow.ActionContext, input workflow.ActionInput) (workflow.ActionResult, error) {
	if ctx == nil || actionContext.Subject.Kind == "" || actionContext.Subject.ID == "" || a.sender == nil || a.database == nil || a.registry == nil || a.dialect == nil {
		return workflow.ActionResult{}, fmt.Errorf("email action is missing its tenant record context")
	}
	if err := validateBuiltinWorkflowSchema(emailWorkflowInputSchema, input.Data); err != nil {
		return workflow.ActionResult{}, err
	}
	var config struct {
		RecipientField string `json:"recipient_field"`
		Subject        string `json:"subject"`
		Message        string `json:"message"`
	}
	if err := json.Unmarshal(input.Data, &config); err != nil {
		return workflow.ActionResult{}, err
	}
	dt := a.registry.Get(actionContext.Subject.Kind)
	if dt == nil || dt.Name != actionContext.Subject.Kind || dt.IsChildTable {
		return workflow.ActionResult{}, contract.NewError(contract.CodeValidationFailed, "workflow subject is not a supported table record")
	}
	field := dt.GetField(config.RecipientField)
	if !isWorkflowEmailField(field) {
		return workflow.ActionResult{}, contract.NewError(contract.CodeValidationFailed, "recipient must be an Email field on the workflow table")
	}
	query := "SELECT " + a.dialect.QuoteIdent(field.Fieldname) + " FROM " + a.dialect.QuoteIdent(dt.RawTableName()) + " WHERE " + a.dialect.QuoteIdent("name") + " = ?"
	var recipient string
	if err := a.database.QueryRowContext(ctx, db.Rebind(a.dialect, query), actionContext.Subject.ID).Scan(&recipient); err != nil {
		return workflow.ActionResult{}, fmt.Errorf("load email recipient from workflow record: %w", err)
	}
	address, err := mail.ParseAddress(strings.TrimSpace(recipient))
	if err != nil || address.Address != strings.TrimSpace(recipient) {
		return workflow.ActionResult{}, contract.NewError(contract.CodeValidationFailed, "record email field does not contain one valid email address")
	}
	if err := a.sender.SendContext(ctx, &email.Message{To: []string{address.Address}, Subject: config.Subject, Body: config.Message}); err != nil {
		// A transport error may arrive after SMTP accepted the message. The
		// workflow runner therefore records an unknown outcome and never
		// automatically retries this delivery.
		return workflow.ActionResult{}, err
	}
	result, _ := json.Marshal(map[string]bool{"sent": true})
	return workflow.ActionResult{Schema: emailWorkflowOutputSchema, Data: result}, nil
}
