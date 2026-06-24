// Package scheduler provides cron-based background job scheduling.
package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/orm"
)

// JobType enumerates the built-in job types.
type JobType string

const (
	JobDoctypeAlert JobType = "doctype_alert"
	JobEmailReport  JobType = "email_report"
	JobWebhook      JobType = "webhook"
)

// JobConfig holds the parsed scheduler configuration.
type JobConfig struct {
	Name     string              `yaml:"name"     json:"name"`
	Type     JobType             `yaml:"type"     json:"type"`
	Schedule string              `yaml:"schedule" json:"schedule"` // Cron expression
	Config   map[string]any      `yaml:"config"   json:"config"`
}

// Scheduler manages and runs background jobs.
type Scheduler struct {
	DB        *sql.DB
	Registry  *doctype.Registry
	TxManager *orm.TxManager
	Email     *email.Sender
	jobs      []*JobConfig
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// New creates a new scheduler.
func New(db *sql.DB, registry *doctype.Registry, txManager *orm.TxManager, emailSender *email.Sender) *Scheduler {
	return &Scheduler{
		DB:        db,
		Registry:  registry,
		TxManager: txManager,
		Email:     emailSender,
	}
}

// RegisterJob adds a job to the scheduler.
func (s *Scheduler) RegisterJob(job *JobConfig) {
	s.jobs = append(s.jobs, job)
}

// Start begins running all registered jobs.
func (s *Scheduler) Start() {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	for _, job := range s.jobs {
		job := job // capture loop variable
		s.wg.Add(1)
		go s.runJob(job)
	}

	slog.Info("scheduler started", "jobs", len(s.jobs))
}

// Stop gracefully shuts down the scheduler.
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	slog.Info("scheduler stopped")
}

func (s *Scheduler) runJob(job *JobConfig) {
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler job panicked", "name", job.Name, "panic", r)
		}
	}()

	cron := parseCron(job.Schedule)
	slog.Info("scheduling job", "name", job.Name, "schedule", job.Schedule, "next", cron.nextRun())

	for {
		waitDuration := time.Until(cron.nextRun())
		if waitDuration < 0 {
			waitDuration = time.Minute
		}

		timer := time.NewTimer(waitDuration)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			slog.Info("running job", "name", job.Name)
			if err := s.executeJob(job); err != nil {
				slog.Error("job failed", "name", job.Name, "error", err)
			}
		}
	}
}

func (s *Scheduler) executeJob(job *JobConfig) error {
	switch job.Type {
	case JobDoctypeAlert:
		return s.runDoctypeAlert(job)
	case JobEmailReport:
		return s.runEmailReport(job)
	default:
		return fmt.Errorf("unknown job type: %s", job.Type)
	}
}

func (s *Scheduler) runDoctypeAlert(job *JobConfig) error {
	doctypeName, _ := job.Config["doctype"].(string)
	if doctypeName == "" {
		return fmt.Errorf("doctype_alert requires 'doctype' in config")
	}

	dt := s.Registry.Get(doctypeName)
	if dt == nil {
		return fmt.Errorf("doctype %q not found", doctypeName)
	}

	// Build filters from config.
	filtersJSON := buildFiltersJSON(job.Config["filters"])
	docs, total, err := s.TxManager.GetList(dt, filtersJSON, "", 500, 0, "")
	if err != nil {
		return fmt.Errorf("querying doctype %s: %w", doctypeName, err)
	}

	if total == 0 {
		slog.Debug("doctype_alert: no matching documents", "job", job.Name)
		return nil
	}

	notifyField, _ := job.Config["notify_field"].(string)
	subject, _ := job.Config["subject"].(string)
	message, _ := job.Config["message"].(string)

	for _, doc := range docs {
		var recipients []string
		if notifyField != "" {
			recipient := doc.GetString(notifyField)
			if recipient != "" {
				recipients = append(recipients, recipient)
			}
		}

		if len(recipients) == 0 {
			continue
		}

		data := docToTemplateData(doc, dt)
		if err := s.Email.SendTemplate(recipients, subject, message, data); err != nil {
			slog.Error("sending alert email", "job", job.Name, "error", err)
		}
	}

	slog.Info("doctype_alert completed", "job", job.Name, "matches", total)
	return nil
}

func (s *Scheduler) runEmailReport(job *JobConfig) error {
	doctypeName, _ := job.Config["doctype"].(string)
	if doctypeName == "" {
		return fmt.Errorf("email_report requires 'doctype' in config")
	}

	dt := s.Registry.Get(doctypeName)
	if dt == nil {
		return fmt.Errorf("doctype %q not found", doctypeName)
	}

	filtersJSON := buildFiltersJSON(job.Config["filters"])
	docs, total, err := s.TxManager.GetList(dt, filtersJSON, "", 500, 0, "")
	if err != nil {
		return fmt.Errorf("querying doctype %s: %w", doctypeName, err)
	}

	subject, _ := job.Config["subject"].(string)
	message, _ := job.Config["message"].(string)

	// Build report body.
	var body strings.Builder
	body.WriteString(message)
	body.WriteString("\n\n")
	for _, doc := range docs {
		title := doc.GetString(dt.TitleField)
		if title == "" {
			title = doc.Name
		}
		body.WriteString(fmt.Sprintf("- %s: %s\n", doc.Name, title))
	}
	body.WriteString(fmt.Sprintf("\nTotal: %d records\n", total))

	// Get recipients from config.
	recipientList, _ := job.Config["recipients"].([]any)
	var recipients []string
	for _, r := range recipientList {
		if rMap, ok := r.(map[string]any); ok {
			if email, ok := rMap["email"].(string); ok && email != "" {
				recipients = append(recipients, email)
			}
		}
	}

	if len(recipients) == 0 {
		slog.Warn("email_report: no recipients", "job", job.Name)
		return nil
	}

	return s.Email.SendTemplate(recipients, subject, body.String(), nil)
}

// cronSchedule represents a parsed cron expression.
type cronSchedule struct {
	minute, hour, dom, month, dow string
}

func parseCron(expr string) *cronSchedule {
	parts := strings.Fields(expr)
	if len(parts) < 5 {
		// Default to every hour if invalid.
		return &cronSchedule{"0", "*", "*", "*", "*"}
	}
	return &cronSchedule{
		minute: parts[0],
		hour:   parts[1],
		dom:    parts[2],
		month:  parts[3],
		dow:    parts[4],
	}
}

// nextRun returns the next time this cron schedule should fire.
// This is a simplified implementation that handles basic patterns.
func (c *cronSchedule) nextRun() time.Time {
	now := time.Now()
	// Simple: fire at the next matching time, or every minute for "*" patterns.
	next := now.Truncate(time.Minute).Add(time.Minute)

	// For specific hour:minute, compute next match.
	if c.hour != "*" && c.minute != "*" {
		var hour, minute int
		fmt.Sscanf(c.hour, "%d", &hour)
		fmt.Sscanf(c.minute, "%d", &minute)

		today := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if today.Before(now) || today.Equal(now) {
			today = today.Add(24 * time.Hour)
		}
		return today
	}

	return next
}

func buildFiltersJSON(filtersRaw any) string {
	if filtersRaw == nil {
		return ""
	}
	// filtersRaw should be a [][]any from YAML.
	// For simplicity, format as JSON string.
	filters, ok := filtersRaw.([]any)
	if !ok {
		return ""
	}

	var parts []string
	for _, f := range filters {
		fArr, ok := f.([]any)
		if !ok || len(fArr) < 3 {
			continue
		}
		field := fmt.Sprintf("%v", fArr[0])
		op := fmt.Sprintf("%v", fArr[1])
		val := fArr[2]

		var valStr string
		switch v := val.(type) {
		case string:
			valStr = fmt.Sprintf(`"%s"`, v)
		default:
			valStr = fmt.Sprintf("%v", v)
		}

		parts = append(parts, fmt.Sprintf(`["%s","%s",%s]`, field, op, valStr))
	}

	return "[" + strings.Join(parts, ",") + "]"
}

func docToTemplateData(doc *doctype.Document, dt *doctype.DocType) map[string]string {
	data := make(map[string]string)
	data["name"] = doc.Name
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			data[f.Fieldname] = fmt.Sprintf("%v", doc.Get(f.Fieldname))
		}
	}
	return data
}

func init() {
	_ = time.Since
}
