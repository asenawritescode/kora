package site

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestTodoPackHasNoLegacyWorkflowConfiguration(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "todo")
	validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "Team Tasks", Version: "1.1.0"}, files)
	if err != nil {
		t.Fatal("validate Team Tasks pack:", err)
	}
	if !validation.Valid {
		t.Fatalf("Team Tasks pack findings: %+v", validation.Findings)
	}

	var task *doctype.DocType
	for _, file := range files {
		if file.Path == "doctypes/todo.yaml" {
			task, err = doctype.ParseYAML([]byte(file.Content), file.Path)
			if err != nil {
				t.Fatal("parse Todo schema:", err)
			}
		}
		if file.ContentType == "workflow_definition" || file.Path == "workflows/todo.json" {
			t.Fatalf("Team Tasks pack still includes retired workflow config %q", file.Path)
		}
	}
	if task == nil {
		t.Fatal("Team Tasks pack is missing the Todo schema")
	}
	assignee := task.GetField("assignee")
	if assignee == nil || assignee.Fieldtype != "User" {
		t.Fatalf("Todo assignee = %#v; want workspace User field", assignee)
	}
	if task.GetField("title") == nil || !task.GetField("title").SearchIndex {
		t.Fatal("Todo title must remain searchable")
	}
}
