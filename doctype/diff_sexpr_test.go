package doctype

import (
	"testing"
)

func TestDiffSExpr_AddField(t *testing.T) {
	oldSExpr := `(config
  (doctypes
    (doctype Task
      (field title
        :required true :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	newSExpr := `(config
  (doctypes
    (doctype Task
      (field description
        :type Text)
      (field title
        :required true :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	changes, err := DiffSExpr(oldSExpr, newSExpr)
	if err != nil {
		t.Fatalf("DiffSExpr error = %v", err)
	}

	foundAdd := false
	for _, c := range changes {
		if c.Type == "add-field" && c.Entity == "Task" && c.Field == "description" {
			foundAdd = true
			break
		}
	}
	if !foundAdd {
		t.Errorf("expected add-field for description in Task, got changes: %+v", changes)
	}
}

func TestDiffSExpr_RemoveDoctype(t *testing.T) {
	oldSExpr := `(config
  (doctypes
    (doctype Task
      (field title
        :type Data))
    (doctype User
      (field name
        :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	newSExpr := `(config
  (doctypes
    (doctype Task
      (field title
        :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	changes, err := DiffSExpr(oldSExpr, newSExpr)
	if err != nil {
		t.Fatalf("DiffSExpr error = %v", err)
	}

	found := false
	for _, c := range changes {
		if c.Type == "remove-doctypes" && c.Entity == "User" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected remove-doctypes for User, got changes: %+v", changes)
	}
}

func TestDiffSExpr_NoChange(t *testing.T) {
	sexpr := `(config
  (doctypes)
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	changes, err := DiffSExpr(sexpr, sexpr)
	if err != nil {
		t.Fatalf("DiffSExpr error = %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no changes, got %d: %+v", len(changes), changes)
	}
}

func TestDiffSExpr_RolePermWorkflow(t *testing.T) {
	oldSExpr := `(config
  (doctypes)
  (roles
    (role Administrator
      :workspace true))
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	newSExpr := `(config
  (doctypes)
  (roles
    (role Administrator
      :description "System admin" :workspace true)
    (role Guest
      :workspace false))
  (permissions
    (perm Task Administrator
      :read true))
  (workflows)
  (analytics-metrics)
  (scripts))`

	changes, err := DiffSExpr(oldSExpr, newSExpr)
	if err != nil {
		t.Fatalf("DiffSExpr error = %v", err)
	}

	if len(changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	modifiedAdmin := false
	addedGuest := false
	addedPerm := false
	for _, c := range changes {
		switch {
		case c.Type == "add-roles" && c.Entity == "Guest":
			addedGuest = true
		case c.Type == "add-permissions" && c.Entity == "Task":
			addedPerm = true
		case c.Type == "modify-roles" && c.Entity == "Administrator":
			modifiedAdmin = true
		}
	}

	if !modifiedAdmin {
		t.Error("expected modify-roles for Administrator")
	}
	if !addedGuest {
		t.Error("expected add-roles for Guest")
	}
	if !addedPerm {
		t.Error("expected add-permissions for Task")
	}
}

func TestDiffSExpr_ModifiedFieldAttr(t *testing.T) {
	oldSExpr := `(config
  (doctypes
    (doctype Task
      (field title
        :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	newSExpr := `(config
  (doctypes
    (doctype Task
      (field title
        :required true :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	changes, err := DiffSExpr(oldSExpr, newSExpr)
	if err != nil {
		t.Fatalf("DiffSExpr error = %v", err)
	}

	found := false
	for _, c := range changes {
		if c.Type == "modify-field" && c.Entity == "Task" && c.Field == "title" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected modify-field for title, got changes: %+v", changes)
	}
}

func TestDiffSExpr_WorkflowStateChange(t *testing.T) {
	oldSExpr := `(config
  (doctypes)
  (roles)
  (permissions)
  (workflows
    (workflow OrderWorkflow
      :active true :on Order
      (state Draft
        :doc-status 0)
      (state Submitted
        :doc-status 1)))
  (analytics-metrics)
  (scripts))`

	newSExpr := `(config
  (doctypes)
  (roles)
  (permissions)
  (workflows
    (workflow OrderWorkflow
      :active true :on Order
      (state Draft
        :doc-status 0)
      (state Approved
        :doc-status 1 :style "success")
      (state Cancelled
        :doc-status 2 :style "danger")))
  (analytics-metrics)
  (scripts))`

	changes, err := DiffSExpr(oldSExpr, newSExpr)
	if err != nil {
		t.Fatalf("DiffSExpr error = %v", err)
	}

	t.Logf("Workflow state changes: %+v", changes)

	removedSubmitted := false
	addedApproved := false
	addedCancelled := false
	for _, c := range changes {
		switch {
		case c.Type == "remove-state" && c.Entity == "OrderWorkflow" && c.Field == "Submitted":
			removedSubmitted = true
		case c.Type == "add-state" && c.Entity == "OrderWorkflow" && c.Field == "Approved":
			addedApproved = true
		case c.Type == "add-state" && c.Entity == "OrderWorkflow" && c.Field == "Cancelled":
			addedCancelled = true
		}
	}

	if !removedSubmitted {
		t.Error("expected remove-state for Submitted")
	}
	if !addedApproved {
		t.Error("expected add-state for Approved")
	}
	if !addedCancelled {
		t.Error("expected add-state for Cancelled")
	}
}

func TestReverseChanges(t *testing.T) {
	changes := []Change{
		{Type: "add-field", Section: "doctypes", Entity: "Task", Field: "description"},
		{Type: "modify-field", Section: "doctypes", Entity: "Task", Field: "title", OldValue: map[string]interface{}{"required": false}, NewValue: map[string]interface{}{"required": true}},
	}
	reversed := ReverseDiff(changes)

	if len(reversed) != 2 {
		t.Fatalf("expected 2 reversed changes, got %d", len(reversed))
	}

	// First reversed change should be the modify (preserved type, swapped values)
	if reversed[0].Type != "modify-field" {
		t.Errorf("expected modify-field, got %s", reversed[0].Type)
	}
	if reversed[0].Field != "title" {
		t.Errorf("expected title field, got %s", reversed[0].Field)
	}

	// Second reversed change should undo the add
	if reversed[1].Type != "remove-field" {
		t.Errorf("expected remove-field, got %s", reversed[1].Type)
	}
	if reversed[1].Field != "description" {
		t.Errorf("expected description field, got %s", reversed[1].Field)
	}
}

func TestDiffSExpr_RenameReuseName(t *testing.T) {
	oldSExpr := `(config
  (doctypes
    (doctype Task
      (field old_name
        :type Data)
      (field shared
        :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	newSExpr := `(config
  (doctypes
    (doctype Task
      (field new_name
        :renamed-from "old_name" :type Data)
      (field shared
        :required true :type Data)))
  (roles)
  (permissions)
  (workflows)
  (analytics-metrics)
  (scripts))`

	changes, err := DiffSExpr(oldSExpr, newSExpr)
	if err != nil {
		t.Fatalf("DiffSExpr error = %v", err)
	}

	t.Logf("Rename+reuse changes: %+v", changes)

	// Should detect: remove old_name, add new_name, modify shared
	removedOld := false
	addedNew := false
	modifiedShared := false
	for _, c := range changes {
		switch {
		case c.Type == "remove-field" && c.Entity == "Task" && c.Field == "old_name":
			removedOld = true
		case c.Type == "add-field" && c.Entity == "Task" && c.Field == "new_name":
			addedNew = true
		case c.Type == "modify-field" && c.Entity == "Task" && c.Field == "shared":
			modifiedShared = true
		}
	}

	if !removedOld {
		t.Error("expected remove-field for old_name")
	}
	if !addedNew {
		t.Error("expected add-field for new_name")
	}
	if !modifiedShared {
		t.Error("expected modify-field for shared")
	}
}
