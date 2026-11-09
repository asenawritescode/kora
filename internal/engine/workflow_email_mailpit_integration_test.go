//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/email"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/go-sql-driver/mysql"
)

func TestWorkflowEmailActionDeliversToMailpitAgainstMySQL(t *testing.T) {
	mailpitURL := strings.TrimRight(envOr("KORA_WORKFLOW_MAILPIT_API", "http://127.0.0.1:8025"), "/")
	mailpitClient := &http.Client{Timeout: time.Second}
	if _, err := getMailpitMessages(t.Context(), mailpitClient, mailpitURL); err != nil {
		t.Skipf("Mailpit is unavailable: %v", err)
	}

	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open MySQL root connection:", err)
	}
	if err := root.PingContext(t.Context()); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	databaseName := fmt.Sprintf("kora_workflow_mailpit_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.ExecContext(t.Context(), "CREATE DATABASE `"+databaseName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated MySQL database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+databaseName+"`")
		_ = root.Close()
	})
	databaseConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse MySQL DSN:", err)
	}
	databaseConfig.DBName = databaseName
	database, err := sql.Open("mysql", databaseConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated MySQL database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	dialect := db.MySQL()
	dt := &doctype.DocType{Name: "Workflow Email Test", Fields: []doctype.Field{{Fieldname: "email", Fieldtype: "Email"}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, nil, nil)
	if _, err := database.ExecContext(ctx, "CREATE TABLE "+dialect.QuoteIdent(dt.RawTableName())+" (name VARCHAR(140) PRIMARY KEY, email VARCHAR(320) NOT NULL)"); err != nil {
		t.Fatal("create workflow test table:", err)
	}
	recipient := "workflow-" + strings.ToLower(contract.NewID()) + "@example.test"
	if _, err := database.ExecContext(ctx, "INSERT INTO "+dialect.QuoteIdent(dt.RawTableName())+" (name,email) VALUES (?,?)", "EMAIL-1", recipient); err != nil {
		t.Fatal("insert workflow test record:", err)
	}

	smtpHost := envOr("KORA_WORKFLOW_TEST_SMTP_HOST", "127.0.0.1")
	smtpPort := 1025
	sender := email.NewSender(&email.Config{Host: smtpHost, Port: smtpPort, TLSMode: "plain", From: "no-reply@kora.local"})
	registration := emailWorkflowRegistration(sender, database, registry, dialect)
	input := workflow.ActionInput{Schema: emailWorkflowInputSchema, Data: json.RawMessage(`{"recipient_field":"email","subject":"Kora workflow delivery check","message":"This message verifies the Engine workflow email action."}`)}
	result, err := registration.External.Execute(ctx, workflow.ActionContext{Subject: contract.SubjectRef{Kind: dt.Name, ID: "EMAIL-1", Revision: "1"}}, input)
	if err != nil {
		t.Fatal("execute registered workflow email action:", err)
	}
	var sent struct {
		Sent bool `json:"sent"`
	}
	if result.Schema != emailWorkflowOutputSchema || json.Unmarshal(result.Data, &sent) != nil || !sent.Sent {
		t.Fatalf("workflow email action result was invalid: schema=%+v", result.Schema)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := getMailpitMessages(ctx, mailpitClient, mailpitURL)
		if err != nil {
			t.Fatal("read Mailpit after email delivery:", err)
		}
		for _, message := range messages {
			for _, to := range message.To {
				if strings.EqualFold(to.Address, recipient) {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Mailpit did not receive the workflow email for the test record")
}

type mailpitMessage struct {
	To []struct {
		Address string `json:"Address"`
	} `json:"To"`
}

func getMailpitMessages(ctx context.Context, client *http.Client, baseURL string) ([]mailpitMessage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/messages?start=0&limit=50", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Mailpit returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Messages []mailpitMessage `json:"messages"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Messages, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
