//go:build integration

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api/ai"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/envcompat"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// Golden questions: Ask Zana answers over a fixed selling dataset, with the model scripted. The script makes the
// tool calls and writes the sentence; everything else is the shipped path: the real metric routes, the answer cards
// the server draws, and the number check. Every card figure is checked against a hand computation from the rows
// below, and a planted wrong number must never reach the person.

const (
	goldenSite       = "tenant-dashboards"
	goldenTenant     = "stable-tenant"
	tillCashierRole  = "Till Cashier"
	goldenAdminUser  = "shop-admin"
	goldenCurrency   = "KES"
	goldenThreadBase = "golden-"
)

// goldenLine is one line of a sale: a product, a quantity and the line total in whole units.
type goldenLine struct {
	product    string
	qty, total int
}

// goldenSale is one sale as the fact consumer receives it. Owner is the cashier who rang it up.
type goldenSale struct {
	record, date, status, owner, customer string
	lines                                 []goldenLine
}

// goldenDates are the business days the dataset is built on: the Monday of last week, of the week before, and today.
type goldenDates struct{ lastMon, weekBeforeMon, today time.Time }

// goldenStart is the start of the dataset's business days, as the tenant clock reads them now. It uses the zone and
// cutoff goldenSettings writes, which are written only when the dataset is seeded, so it must not read the stored clock.
func goldenStart(t *testing.T) goldenDates {
	t.Helper()
	loc, err := tenanttime.ParseZone("Africa/Nairobi")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	cutoff, err := tenanttime.ParseCutoff("04:00")
	if err != nil {
		t.Fatalf("cutoff: %v", err)
	}
	bd := tenanttime.Clock{Location: loc, Cutoff: cutoff}.BusinessDate(time.Now())
	today := time.Date(bd.Year, bd.Month, bd.Day, 0, 0, 0, 0, time.UTC)
	thisMon := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	return goldenDates{lastMon: thisMon.AddDate(0, 0, -7), weekBeforeMon: thisMon.AddDate(0, 0, -14), today: today}
}

// goldenDataset is the selling day the questions are checked against. Last week's checked sales (Paid, Partially Paid
// and Awaiting Payment) add up to 1,820 over four sales; the Return on Sunday is not a sale. The week before has 750.
// Today has two sales of 300 and 2 receipts, so the average sale is 150.
func goldenDataset(d goldenDates) []goldenSale {
	at := func(base time.Time, n int) string { return base.AddDate(0, 0, n).Format(time.DateOnly) }
	l, w, t := d.lastMon, d.weekBeforeMon, d.today
	return []goldenSale{
		{record: "SAL-L1", date: at(l, 0), status: "Paid", owner: "alice", customer: "CUST-1",
			lines: []goldenLine{{"PROD-1", 2, 300}, {"PROD-2", 1, 150}}}, // 450, Customer 1
		{record: "SAL-L2", date: at(l, 1), status: "Paid", owner: "bob", customer: "CUST-2",
			lines: []goldenLine{{"PROD-1", 1, 150}}}, // 150, Customer 2
		{record: "SAL-L3", date: at(l, 2), status: "Partially Paid", owner: "alice", customer: "CUST-1",
			lines: []goldenLine{{"PROD-3", 3, 900}}}, // 900, Customer 1
		{record: "SAL-L4", date: at(l, 5), status: "Awaiting Payment", owner: "bob", customer: "CUST-2",
			lines: []goldenLine{{"PROD-2", 2, 320}}}, // 320, Customer 2
		{record: "SAL-L5", date: at(l, 6), status: "Return", owner: "alice", customer: "CUST-1",
			lines: []goldenLine{{"PROD-1", -1, -150}}}, // -150, not a sale
		{record: "SAL-W1", date: at(w, 2), status: "Paid", owner: "bob", customer: "CUST-1",
			lines: []goldenLine{{"PROD-1", 4, 600}}}, // 600
		{record: "SAL-W2", date: at(w, 4), status: "Paid", owner: "alice", customer: "CUST-2",
			lines: []goldenLine{{"PROD-2", 1, 150}}}, // 150
		{record: "SAL-T1", date: at(t, 0), status: "Paid", owner: "alice", customer: "CUST-1",
			lines: []goldenLine{{"PROD-1", 1, 200}}}, // 200
		{record: "SAL-T2", date: at(t, 0), status: "Paid", owner: "bob", customer: "CUST-2",
			lines: []goldenLine{{"PROD-3", 1, 100}}}, // 100
	}
}

// seedGoldenSales creates the tables the metrics read, syncs the fact tables, and delivers every sale to the fact
// consumer, the way the event stream does. The metric routes then answer from the consumed rows.
func seedGoldenSales(ctx context.Context, t *testing.T, database *sql.DB, dialect string, reg *doctype.Registry, sales []goldenSale) {
	t.Helper()
	d := kdb.Resolve(dialect)
	setup := []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, reference VARCHAR(140) NOT NULL, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, sale_date DATE NOT NULL, status VARCHAR(140) NOT NULL, customer VARCHAR(140) NOT NULL, total DECIMAL(18,2) NOT NULL, base_total DECIMAL(18,2) NOT NULL, cost_total DECIMAL(18,2) NOT NULL, profit DECIMAL(18,2) NOT NULL, discount_amount DECIMAL(18,2) NOT NULL, tax_amount DECIMAL(18,2) NOT NULL)", d.QuoteIdent("tabSale")),
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, parent VARCHAR(140) NOT NULL, parentfield VARCHAR(140) NOT NULL, product VARCHAR(140) NOT NULL, quantity DECIMAL(18,2) NOT NULL, line_total DECIMAL(18,2) NOT NULL, line_cost DECIMAL(18,2) NOT NULL, line_profit DECIMAL(18,2) NOT NULL)", d.QuoteIdent("tabSale__items")),
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, status VARCHAR(140) NOT NULL)", d.QuoteIdent("tabCustomer")),
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, product_name VARCHAR(140) NOT NULL)", d.QuoteIdent("tabProduct")),
		"CREATE TABLE IF NOT EXISTS _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
	}
	for _, stmt := range setup {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	settings := kdb.Rebind(d, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)")
	for _, kv := range [][2]string{{tenanttime.KeyTimeZone, "Africa/Nairobi"}, {tenanttime.KeyBusinessDayCutoff, "04:00"}} {
		if _, err := database.ExecContext(ctx, settings, goldenSite, kv[0], kv[1]); err != nil {
			t.Fatalf("site setting %s: %v", kv[0], err)
		}
	}
	// The two cashiers of the sales: alice has a full name, bob only an email, so his row is labelled by it.
	for _, u := range [][3]string{{"alice", "alice@shop.test", "Alice Wanjiru"}, {"bob", "bob@shop.test", ""}} {
		if _, err := database.ExecContext(ctx, kdb.Rebind(d, "INSERT INTO _kora_user (name, site, email, password_hash, full_name) VALUES (?, ?, ?, 'x', ?)"), u[0], goldenSite, u[1], u[2]); err != nil {
			t.Fatalf("seed user %s: %v", u[0], err)
		}
	}
	for _, row := range [][2]string{{"CUST-1", "Active"}, {"CUST-2", "Inactive"}} {
		if _, err := database.ExecContext(ctx, kdb.Rebind(d, fmt.Sprintf("INSERT INTO %s (name, status) VALUES (?, ?)", d.QuoteIdent("tabCustomer"))), row[0], row[1]); err != nil {
			t.Fatalf("customer %s: %v", row[0], err)
		}
	}
	for _, row := range [][2]string{{"PROD-1", "Soda"}, {"PROD-2", "Juice"}, {"PROD-3", "Bread"}} {
		if _, err := database.ExecContext(ctx, kdb.Rebind(d, fmt.Sprintf("INSERT INTO %s (name, product_name) VALUES (?, ?)", d.QuoteIdent("tabProduct"))), row[0], row[1]); err != nil {
			t.Fatalf("product %s: %v", row[0], err)
		}
	}
	if err := (&facts.ModelSync{DB: database, Dialect: d, Registry: reg, Owner: "ask-zana-golden"}).Sync(ctx); err != nil {
		t.Fatalf("sync fact models: %v", err)
	}
	builder := &facts.Builder{DB: database, Dialect: d, Registry: reg}
	for i, s := range sales {
		insertGoldenSourceSale(ctx, t, database, d, s)
		consumeGoldenSale(ctx, t, database, builder, int64(i+1), s)
	}
	refresher := &facts.Refresher{DB: database, Dialect: d, Registry: reg, Admission: stockAdmission{}, Owner: "ask-zana-golden"}
	if _, err := refresher.RefreshDue(ctx); err != nil {
		t.Fatalf("refresh summaries: %v", err)
	}
	// The rebuild the sync queued runs now, from the source rows, so the history reads ready.
	drainGoldenRebuild(ctx, t, database, dialect, reg)
}

// drainGoldenRebuild runs the queued rebuild of the Sale model from its source rows.
func drainGoldenRebuild(ctx context.Context, t *testing.T, database *sql.DB, dialect string, reg *doctype.Registry) {
	t.Helper()
	saleOnly := doctype.NewRegistry()
	saleOnly.Register(reg.Get("Sale"))
	if _, err := (&facts.RebuildRunner{DB: database, Dialect: kdb.Resolve(dialect), Registry: saleOnly, Site: goldenSite, Owner: "ask-zana-golden"}).Drain(ctx); err != nil {
		t.Fatalf("rebuild fact models: %v", err)
	}
}

// insertGoldenSourceSale writes a sale and its lines as the source tables hold them, for the rebuild to read.
func insertGoldenSourceSale(ctx context.Context, t *testing.T, database *sql.DB, d kdb.Dialect, s goldenSale) {
	t.Helper()
	total := 0
	for _, l := range s.lines {
		total += l.total
	}
	amount := strconv.Itoa(total)
	header := kdb.Rebind(d, fmt.Sprintf("INSERT INTO %s (name, reference, owner, creation, sale_date, status, customer, total, base_total, cost_total, profit, discount_amount, tax_amount) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, 0)", d.QuoteIdent("tabSale")))
	if _, err := database.ExecContext(ctx, header, s.record, s.record, s.owner, s.date+" 10:00:00", s.date, s.status, s.customer, amount, amount, amount); err != nil {
		t.Fatalf("insert %s: %v", s.record, err)
	}
	for i, l := range s.lines {
		line := kdb.Rebind(d, fmt.Sprintf("INSERT INTO %s (name, parent, parentfield, product, quantity, line_total, line_cost, line_profit) VALUES (?, ?, 'items', ?, ?, ?, 0, ?)", d.QuoteIdent("tabSale__items")))
		if _, err := database.ExecContext(ctx, line, fmt.Sprintf("%s-%d", s.record, i+1), s.record, l.product, strconv.Itoa(l.qty), strconv.Itoa(l.total), strconv.Itoa(l.total)); err != nil {
			t.Fatalf("insert line of %s: %v", s.record, err)
		}
	}
}

// consumeGoldenSale delivers one sale's insert, with its lines, to the fact consumer in its own transaction.
func consumeGoldenSale(ctx context.Context, t *testing.T, database *sql.DB, b *facts.Builder, seq int64, s goldenSale) {
	t.Helper()
	items := []any{}
	total := 0
	for i, l := range s.lines {
		total += l.total
		items = append(items, map[string]any{"name": fmt.Sprintf("%s-%d", s.record, i+1), "product": l.product,
			"quantity": strconv.Itoa(l.qty), "line_total": strconv.Itoa(l.total), "line_cost": "0", "line_profit": strconv.Itoa(l.total)})
	}
	amount := strconv.Itoa(total)
	payload := map[string]any{
		"table": "Sale", "record_id": s.record,
		"data": map[string]any{"sale_date": s.date, "status": s.status, "customer": s.customer, "total": amount,
			"base_total": amount, "cost_total": "0", "profit": amount, "discount_amount": "0", "tax_amount": "0", "items": items},
	}
	event := contract.EventEnvelope{
		ID: fmt.Sprintf("evt-sale-%d", seq), Type: outbox.EventTypeName("Sale", eventstream.EventInsert),
		Sequence: contract.Revision(strconv.FormatInt(seq, 10)), Site: goldenSite,
		OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		Data:       contract.MustEncodeData(payload),
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Consume(ctx, tx, event); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume %s: %v", s.record, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// askZanaRouter mounts the chat and the metric routes for a caller named by X-User and X-Roles, on the real handlers.
func askZanaRouter(t *testing.T, database *sql.DB, reg *doctype.Registry, dialect string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		user := c.GetHeader("X-User")
		actor := contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user,
			Site: goldenSite, Roles: strings.Split(c.GetHeader("X-Roles"), ","), AuthenticatedAt: time.Now().UTC()}
		execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(goldenTenant), actor, nil)
		if err != nil {
			t.Errorf("verified execution: %v", err)
		}
		c.Set("site_name", goldenSite)
		c.Set("site_id", goldenTenant)
		c.Set("site_registry", reg)
		c.Set("site_db", database)
		c.Set("site_db_type", dialect)
		c.Set("policy_execution", execution)
		c.Next()
	})
	metrics := registerMetricsRoutes(router.Group("/api/v1"), reg, database, kdb.Resolve(dialect))
	h := &Handler{Registry: reg, metrics: metrics}
	router.POST("/api/v1/ai/chat", h.HandleChat)
	return router
}

// modelStep is one scripted provider answer: a tool call with its JSON arguments, or else the reply text.
type modelStep struct {
	tool, args, content string
	// finish overrides the finish reason of a reply; empty is "stop".
	finish string
}

// fakeModel is the chat provider: it answers each request with the next scripted step and keeps every request body.
type fakeModel struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	steps  []modelStep
	bodies []string
}

// startFakeModel serves the scripted provider on a local HTTP server, closed with the test.
func startFakeModel(t *testing.T) *fakeModel {
	t.Helper()
	m := &fakeModel{t: t}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

// script replaces the steps the next turn consumes. Steps left over from the last turn are a test error.
func (m *fakeModel) script(steps []modelStep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.steps) > 0 {
		m.t.Errorf("%d scripted model steps were not used", len(m.steps))
	}
	m.steps = steps
}

// served is how many requests the provider has answered so far.
func (m *fakeModel) served() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

// since returns the request bodies answered after the first n.
func (m *fakeModel) since(n int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies[n:]...)
}

// serve answers one provider request with the next scripted step, in the OpenAI chat completion shape.
func (m *fakeModel) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies = append(m.bodies, string(raw))
	if len(m.steps) == 0 {
		m.t.Errorf("the model was asked again with nothing scripted: %s", raw)
		http.Error(w, "nothing scripted", http.StatusInternalServerError)
		return
	}
	step := m.steps[0]
	m.steps = m.steps[1:]
	message := map[string]any{"role": "assistant", "content": step.content}
	finish := "stop"
	if step.finish != "" {
		finish = step.finish
	}
	if step.tool != "" {
		finish = "tool_calls"
		message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": fmt.Sprintf("call_%d", len(m.bodies)), "type": "function",
			"function": map[string]any{"name": step.tool, "arguments": step.args},
		}}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
	})
}

// goldenTurn is one message the person sends, the steps the model takes for it, and what must hold afterwards.
type goldenTurn struct {
	message string
	steps   []modelStep
	check   func(t *testing.T, got ai.ChatResponse, requests []string)
}

// goldenCase is one question: who asks it and its turns. Each case has its own conversation. A building case
// queues a rebuild of the sales history before its turns and runs it after them.
type goldenCase struct {
	name     string
	user     string
	roles    string
	building bool
	turns    []goldenTurn
}

// createGoldenReport saves "Last week sales" for the shop, as the administrator, through the reports API, and
// returns its id. The shop's report is visible to everyone who can read Sale.
func createGoldenReport(t *testing.T, router *gin.Engine) string {
	t.Helper()
	body := `{"name":"Last week sales","visibility":"shop","question":{"metric":"sales","period":"last_week","grain":"range"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/explore/reports", strings.NewReader(body))
	req.Header.Set("X-User", goldenAdminUser)
	req.Header.Set("X-Roles", doctype.AdminRole)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create saved report = %d: %s", recorder.Code, recorder.Body)
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil || env.Data.ID == "" {
		t.Fatalf("create saved report: %v: %s", err, recorder.Body)
	}
	return env.Data.ID
}

// askZana sends one message as user and returns the answer the chat wrote.
func askZana(t *testing.T, router *gin.Engine, user, roles, thread, message string) ai.ChatResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"message": message, "context": map[string]any{"threadId": thread}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", user)
	req.Header.Set("X-Roles", roles)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("chat %q = %d: %s", message, recorder.Code, recorder.Body)
	}
	var env struct {
		Data ai.ChatResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
		t.Fatal("decode chat answer:", err)
	}
	return env.Data
}

// toolResults returns the content of every tool message in a provider request.
func toolResults(t *testing.T, body string) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal("decode provider request:", err)
	}
	var out []string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

// spanOf prints an inclusive span of business dates as the cards do: "9 Nov – 15 Nov 2026".
func spanOf(from, to time.Time) string {
	return fmt.Sprintf("%d %s – %d %s %d", from.Day(), from.Format("Jan"), to.Day(), to.Format("Jan"), to.Year())
}

// requireCards checks the answer carries exactly n cards and returns them.
func requireCards(t *testing.T, got ai.ChatResponse, n int) []contract.AnswerCard {
	t.Helper()
	if len(got.Cards) != n {
		t.Fatalf("cards = %d (reply %q), want %d", len(got.Cards), got.Reply, n)
	}
	return got.Cards
}

// rowValues returns each table row as "label value", in order.
func rowValues(card contract.AnswerCard) []string {
	out := make([]string, 0, len(card.Rows))
	for _, row := range card.Rows {
		out = append(out, row.Label+" "+row.Value)
	}
	return out
}

// sameSet reports whether two lists hold the same values, in any order.
func sameSet(got, want []string) bool {
	a, b := slices.Clone(got), slices.Clone(want)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// goldenCases are the questions, each with the answer the dataset gives. The expected figures are the hand
// computations above; the replies are what the model would say, which the server must let through or replace.
func goldenCases(d goldenDates, reportID string) []goldenCase {
	lastWeek := spanOf(d.lastMon, d.lastMon.AddDate(0, 0, 6))
	weekBefore := spanOf(d.weekBeforeMon, d.weekBeforeMon.AddDate(0, 0, 6))
	lastWeekQuery := `{"metrics":["sales"],"period":{"preset":"last_week"},"grain":"range"}`
	salesCompare := `{"metrics":["sales"],"period":{"preset":"last_week"},"compare":"week_before","grain":"range"}`
	weekBeforeQuery := fmt.Sprintf(`{"metrics":["sales"],"from":%q,"to":%q,"grain":"range"}`,
		d.weekBeforeMon.Format(time.DateOnly), d.weekBeforeMon.AddDate(0, 0, 6).Format(time.DateOnly))
	clarify := `{"question":"Which sales do you mean?","choices":[{"label":"Number of sales","hint":"How many sales were made"},{"label":"Sales total","hint":"The base total of the sales"}]}`
	productQuery := `{"metrics":["items_sold"],"dimensions":["items.product"],"top":5,"period":{"preset":"last_week"},"grain":"range"}`
	drill := fmt.Sprintf(`{"metric":"sales","from":%q,"to":%q}`, d.lastMon.Format(time.DateOnly), d.lastMon.AddDate(0, 0, 6).Format(time.DateOnly))
	lineByProduct := `{"measures":[{"record_type":"Sale","agg":"sum","field":"line_total"}],"dimensions":["items.product"],"period":{"preset":"last_week"},"grain":"range"}`
	sodaDrill := fmt.Sprintf(`{"measure":{"record_type":"Sale","agg":"sum","field":"line_total"},"from":%q,"to":%q,"group":{"items.product":"PROD-1"}}`,
		d.lastMon.Format(time.DateOnly), d.lastMon.AddDate(0, 0, 6).Format(time.DateOnly))
	const wrongOnce = "Sales last week were KES 1,821."
	const wrongAgain = "Sales last week were KES 1,822."
	const rightReply = "Sales last week were KES 1,820."
	return []goldenCase{
		{name: "sales last week against the week before", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "How did sales do last week compared with the week before?",
			steps:   []modelStep{{tool: "query_metrics", args: salesCompare}, {content: "Sales last week were KES 1,820, up 142.7% on the week before."}},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				if card.Kind != "number" || card.Value != "KES 1,820" || card.Previous != "KES 750" || card.Change != "▲ 142.7% on the week before" {
					t.Fatalf("card = %+v", card)
				}
				if card.PreviousLabel != "Week before" || card.Meta.DayEnds != "days end 04:00" {
					t.Fatalf("previous label %q, number card day cutoff %q; want %q and the cutoff", card.PreviousLabel, card.Meta.DayEnds, "Week before")
				}
				if !card.Verified || card.ScopeNote != "" || card.Meta.Period != lastWeek || card.Meta.Compared != weekBefore {
					t.Fatalf("card flags or dates = %+v", card)
				}
				// The sparkline is the day-by-day read: its days add back to the figure, and the sales of
				// Monday 450, Tuesday 150, Wednesday 900 and Saturday 320 are among them.
				sum := 0
				for _, v := range card.Sparkline {
					n, err := strconv.Atoi(v)
					if err != nil {
						t.Fatalf("sparkline value %q: %v", v, err)
					}
					sum += n
				}
				if sum != 1820 || !slices.Contains(card.Sparkline, "450") || !slices.Contains(card.Sparkline, "320") {
					t.Fatalf("sparkline = %v; want the seven days adding to 1,820", card.Sparkline)
				}
			},
		}}},
		{name: "top five products", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "What were my top 5 products last week?",
			steps: []modelStep{
				{tool: "query_metrics", args: productQuery},
				{content: "Soda, Juice and Bread each sold 3 last week."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				want := []string{"Soda 3", "Juice 3", "Bread 3"}
				if card.Kind != "table" || !card.Verified || !sameSet(rowValues(card), want) {
					t.Fatalf("rows = %v (kind %s verified %v), want %v verified", rowValues(card), card.Kind, card.Verified, want)
				}
				// A table carries the records behind it, named for the lines it groups, and no day cutoff in its meta.
				var see *contract.AnswerAction
				for i := range card.Actions {
					if card.Actions[i].Kind == "see_records" {
						see = &card.Actions[i]
					}
				}
				if see == nil || see.Label != "See the sales lines" || card.Meta.DayEnds != "" {
					t.Fatalf("actions = %+v day cutoff %q; want see_records \"See the sales lines\" and no day cutoff", card.Actions, card.Meta.DayEnds)
				}
			},
		}}},
		{name: "sales by cashier are labelled with the cashiers' names", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "What were my sales by cashier last week?",
			steps: []modelStep{
				{tool: "query_metrics", args: `{"metrics":["sales"],"dimensions":["owner"],"period":{"preset":"last_week"},"grain":"range"}`},
				{content: "Alice Wanjiru sold KES 1,350 and bob@shop.test sold KES 470 last week."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				want := []string{"Alice Wanjiru KES 1,350", "bob@shop.test KES 470"}
				if card.Kind != "table" || !card.Verified || !sameSet(rowValues(card), want) {
					t.Fatalf("rows = %v (kind %s), want %v", rowValues(card), card.Kind, want)
				}
			},
		}}},
		{name: "one figure for one measure is a number card even when two metrics are asked", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "How much did we sell today?",
			steps: []modelStep{
				{tool: "query_metrics", args: `{"metrics":["sales","receipts"],"period":{"preset":"today"},"grain":"range"}`},
				{content: "Sales today were KES 300."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				if card.Kind != "number" || card.Label != "Sales" || card.Value != "KES 300" || len(card.Rows) != 0 {
					t.Fatalf("card = %+v; want one number card for sales KES 300", card)
				}
			},
		}}},
		{name: "average sale today", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "What was my average sale today?",
			steps: []modelStep{
				{tool: "query_metrics", args: `{"metrics":["average_sale"],"period":{"preset":"today"},"grain":"range"}`},
				{content: "Average sale today was KES 150."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				if card.Kind != "number" || card.Label != "Average sale" || card.Value != "KES 150" || !card.Verified {
					t.Fatalf("card = %+v", card)
				}
			},
		}}},
		{name: "sales split by product is refused and the line measure is offered", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "What were my sales by product last week?",
			steps: []modelStep{
				{tool: "query_metrics", args: `{"metrics":["sales"],"dimensions":["items.product"],"period":{"preset":"last_week"},"grain":"range"}`},
				{tool: "query_metrics", args: productQuery},
				{content: "A sale total cannot be split by product, so these are the quantities sold by product: Soda 3, Juice 3, Bread 3."},
			},
			check: func(t *testing.T, got ai.ChatResponse, requests []string) {
				results := toolResults(t, requests[1])
				if len(results) != 1 || !strings.Contains(results[0], "use a line measure") || strings.Contains(results[0], `"rows"`) {
					t.Fatalf("refusal = %q; want the typed refusal with no rows", results)
				}
				card := requireCards(t, got, 1)[0]
				want := []string{"Soda 3", "Juice 3", "Bread 3"}
				if card.Kind != "table" || !card.Verified || !sameSet(rowValues(card), want) {
					t.Fatalf("rows = %v, want the verified quantities %v", rowValues(card), want)
				}
			},
		}}},
		{name: "sales by the customer's status through a link", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "Which customers bought the most last week, active or inactive?",
			steps: []modelStep{
				{tool: "query_metrics", args: `{"metrics":["sales"],"dimensions":["customer.status"],"period":{"preset":"last_week"},"grain":"range"}`},
				{content: "Active customers bought KES 1,350 last week, inactive ones KES 470."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				want := []string{"Active KES 1,350", "Inactive KES 470"}
				if card.Kind != "table" || strings.Join(rowValues(card), "|") != strings.Join(want, "|") {
					t.Fatalf("rows = %v, want %v", rowValues(card), want)
				}
			},
		}}},
		{name: "sales by status", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "Break last week's sales down by status.",
			steps: []modelStep{
				{tool: "query_metrics", args: `{"metrics":["sales"],"dimensions":["status"],"period":{"preset":"last_week"},"grain":"range"}`},
				{content: "Partially paid sales were KES 900 last week."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				// The three checked statuses are the sales; a Return group may appear, at zero, beside them.
				want := []string{"Paid KES 600", "Partially Paid KES 900", "Awaiting Payment KES 320"}
				seen := map[string]bool{}
				for _, row := range rowValues(card) {
					seen[row] = true
				}
				for _, w := range want {
					if !seen[w] {
						t.Fatalf("rows = %v, want %v among them", rowValues(card), want)
					}
				}
				for _, row := range rowValues(card) {
					if !slices.Contains(want, row) && row != "Return KES 0" {
						t.Fatalf("unexpected row %q in sales by status", row)
					}
				}
			},
		}}},
		{name: "question with no fitting metric gives no number", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "What is my margin after rent?",
			steps: []modelStep{
				{tool: "list_metrics", args: `{}`},
				{content: "I can't work out your margin after rent exactly. Zana has no rent recorded for your shop, and it won't guess."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				requireCards(t, got, 0)
				if strings.ContainsAny(got.Reply, "0123456789") || got.Choices != nil {
					t.Fatalf("refusal = %q choices %v; want no number and no choices", got.Reply, got.Choices)
				}
			},
		}}},
		{name: "a thing the shop does not measure is refused without a period question", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "How many unicorns did we sell?",
			steps: []modelStep{
				{tool: "list_metrics", args: `{}`},
				{content: "Zana cannot answer that from this shop's data: no unicorn sales are recorded here."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				requireCards(t, got, 0)
				if strings.ContainsAny(got.Reply, "0123456789") || got.Choices != nil {
					t.Fatalf("refusal = %q choices %v; want no number and no choices", got.Reply, got.Choices)
				}
			},
		}}},
		{name: "a measure with no metric is not called untracked", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "How much tax did we collect last month?",
			steps: []modelStep{
				{tool: "list_metrics", args: `{}`},
				{content: "This shop has no tax metric, so Zana cannot give the tax collected last month."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				requireCards(t, got, 0)
				if strings.ContainsAny(got.Reply, "0123456789") || got.Choices != nil || strings.Contains(got.Reply, "not tracked") || strings.Contains(got.Reply, "not recorded") {
					t.Fatalf("refusal = %q choices %v; want no number, no choices and no claim the data is untracked", got.Reply, got.Choices)
				}
				if !strings.Contains(got.Reply, "no tax metric") {
					t.Fatalf("refusal = %q; want it to name the missing metric", got.Reply)
				}
			},
		}}},
		{name: "cashier with owner-only access sees only own sales", user: "alice", roles: tillCashierRole, turns: []goldenTurn{{
			message: "How did sales do last week?",
			steps:   []modelStep{{tool: "query_metrics", args: lastWeekQuery}, {content: "Your sales last week were KES 1,350."}},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				if card.Value != "KES 1,350" || card.ScopeNote != "Only your own records" {
					t.Fatalf("card = %+v; want the cashier's own 1,350 with the scope note", card)
				}
			},
		}}},
		{name: "saved report is run and labelled as unchecked", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "What did the saved last week sales report say?",
			steps: []modelStep{
				{tool: "query_metrics", args: fmt.Sprintf(`{"saved_report":%q}`, reportID)},
				{content: "Last week sales, from the report saved by shop-admin, were KES 1,820."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				if card.Verified || card.Value != "KES 1,820" || card.Label != "Last week sales (saved by shop-admin)" {
					t.Fatalf("card = %+v; want an unchecked 1,820 named for the saved report", card)
				}
			},
		}}},
		{name: "history still building gives no number", user: goldenAdminUser, roles: doctype.AdminRole, building: true, turns: []goldenTurn{{
			message: "How did sales do last week?",
			steps: []modelStep{
				{tool: "query_metrics", args: lastWeekQuery},
				{content: "The history for sales is still building, so I can't give that figure yet."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				if card.Kind != "building" || card.Value != "" || card.Previous != "" || !strings.Contains(card.Change, "Still building history") {
					t.Fatalf("card = %+v; want a building card with no figure", card)
				}
				if strings.ContainsAny(got.Reply, "0123456789") {
					t.Fatalf("reply %q states a number while the history builds", got.Reply)
				}
			},
		}}},
		{name: "unchecked measure is labelled as not checked", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "Add up the base amounts of last week's sales records.",
			steps: []modelStep{
				{tool: "query_metrics", args: `{"measures":[{"record_type":"Sale","agg":"sum","field":"base_total"}],"period":{"preset":"last_week"},"grain":"range"}`},
				{content: "Last week's base amounts add up to KES 1,670."},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				card := requireCards(t, got, 1)[0]
				if card.Verified || card.Value != "KES 1,670" {
					t.Fatalf("card = %+v; want an unchecked 1,670", card)
				}
				// The question can be saved as a report in its ad-hoc form, so the save action carries it.
				for _, a := range card.Actions {
					if a.Kind == "save_as_report" && strings.Contains(string(a.Question), `"ad_hoc"`) {
						return
					}
				}
				t.Fatalf("actions = %+v; want save_as_report with the ad-hoc question", card.Actions)
			},
		}}},
		{name: "follow-up moves the period and keeps the question", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{
			{
				message: "How did sales do last week?",
				steps:   []modelStep{{tool: "query_metrics", args: lastWeekQuery}, {content: "Sales last week were KES 1,820."}},
				check: func(t *testing.T, got ai.ChatResponse, _ []string) {
					requireCards(t, got, 1)
				},
			},
			{
				message: "And the week before that?",
				steps:   []modelStep{{tool: "query_metrics", args: weekBeforeQuery}, {content: "For the week before that, sales were KES 750."}},
				check: func(t *testing.T, got ai.ChatResponse, requests []string) {
					if !strings.Contains(requests[0], "PREVIOUS METRICS QUESTION") {
						t.Fatal("the follow-up did not carry the last question to the model")
					}
					card := requireCards(t, got, 1)[0]
					if card.Value != "KES 750" || card.Meta.Period != weekBefore {
						t.Fatalf("card = %+v; want 750 for %s", card, weekBefore)
					}
				},
			},
		}},
		{name: "clarifying choice then the answer", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{
			{
				message: "Show me sales for last week.",
				steps:   []modelStep{{tool: "ask_choice", args: clarify}},
				check: func(t *testing.T, got ai.ChatResponse, _ []string) {
					requireCards(t, got, 0)
					if got.Choices == nil || got.Choices.Question != "Which sales do you mean?" || len(got.Choices.Choices) != 2 || got.Reply != got.Choices.Question {
						t.Fatalf("choices = %+v reply %q", got.Choices, got.Reply)
					}
				},
			},
			{
				message: "Number of sales",
				steps: []modelStep{
					{tool: "query_metrics", args: `{"metrics":["receipts"],"period":{"preset":"last_week"},"grain":"range"}`},
					{content: "There were 4 sales last week."},
				},
				check: func(t *testing.T, got ai.ChatResponse, _ []string) {
					card := requireCards(t, got, 1)[0]
					if card.Label != "Receipts" || card.Value != "4" || !card.Verified {
						t.Fatalf("card = %+v; want 4 receipts", card)
					}
				},
			},
		}},
		{name: "a wrong number is sent back once and corrected", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "How did sales do last week?",
			steps: []modelStep{
				{tool: "query_metrics", args: lastWeekQuery},
				{content: wrongOnce},
				{content: rightReply},
			},
			check: func(t *testing.T, got ai.ChatResponse, requests []string) {
				if got.Reply != rightReply {
					t.Fatalf("reply = %q, want the corrected sentence", got.Reply)
				}
				if !strings.Contains(requests[2], "not in the query results: 1,821") {
					t.Fatal("the correction did not name the wrong figure")
				}
				requireCards(t, got, 1)
			},
		}}},
		{name: "a wrong number twice is replaced by a sentence with no number", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "How did sales do last week?",
			steps: []modelStep{
				{tool: "query_metrics", args: lastWeekQuery},
				{content: wrongOnce},
				{content: wrongAgain},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				if got.Reply != "Here are sales for last week." {
					t.Fatalf("reply = %q, want the numberless sentence", got.Reply)
				}
				requireCards(t, got, 1)
			},
		}}},
		{name: "a reply cut at the length limit is checked too", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "How did sales do last week?",
			steps: []modelStep{
				{tool: "query_metrics", args: lastWeekQuery},
				{content: wrongOnce, finish: "length"},
			},
			check: func(t *testing.T, got ai.ChatResponse, _ []string) {
				if got.Reply != "Here are sales for last week." {
					t.Fatalf("reply = %q, want the numberless sentence", got.Reply)
				}
				requireCards(t, got, 1)
			},
		}}},
		{name: "drill total equals the number", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "Show me the sales records behind last week's total.",
			steps: []modelStep{
				{tool: "query_metrics", args: lastWeekQuery},
				{tool: "drill_metrics", args: drill},
				{content: "Sales last week were KES 1,820, and the records behind it add up to the same KES 1,820."},
			},
			check: func(t *testing.T, got ai.ChatResponse, requests []string) {
				total := regexp.MustCompile(`"total":"1820(\.0+)?"`)
				results := toolResults(t, requests[2])
				if len(results) != 2 || !total.MatchString(results[1]) {
					t.Fatalf("tool results = %q; want the query and a drill with total 1820", results)
				}
				requireCards(t, got, 1)
			},
		}}},
		{name: "one product's sales drill to its lines", user: goldenAdminUser, roles: doctype.AdminRole, turns: []goldenTurn{{
			message: "Show me the sales of Soda last week.",
			steps: []modelStep{
				{tool: "query_metrics", args: lineByProduct},
				{tool: "drill_metrics", args: sodaDrill},
				{content: "Soda sold KES 300 last week, and its sale lines add up to the same KES 300."},
			},
			check: func(t *testing.T, got ai.ChatResponse, requests []string) {
				total := regexp.MustCompile(`"total":"300(\.0+)?"`)
				results := toolResults(t, requests[2])
				if len(results) != 2 || !total.MatchString(results[1]) {
					t.Fatalf("tool results = %q; want the line sums and a drill of Soda with total 300", results)
				}
			},
		}}},
	}
}

// TestAskZanaGoldenQuestions asks Zana fixed questions over the selling dataset on MySQL and PostgreSQL, with the
// model scripted, and checks every card figure and every reply against the hand computations.
func TestAskZanaGoldenQuestions(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_ASK_ZANA_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_ASK_ZANA_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			captureLogs(t)
			database, reg := openErpsell(t, tc.dialect, dsn,
				[]*doctype.Role{{Name: tillCashierRole}},
				[]*doctype.Permission{{Doctype: "Sale", Role: tillCashierRole, Read: true, IfOwner: true}})
			dates := goldenStart(t)
			seedGoldenSales(context.Background(), t, database, tc.dialect, reg, goldenDataset(dates))
			reportID := createGoldenReport(t, askZanaRouter(t, database, reg, tc.dialect))

			model := startFakeModel(t)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", model.server.URL)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "")
			t.Setenv(envcompat.IntegrationTokenEnv, "golden-token")
			router := askZanaRouter(t, database, reg, tc.dialect)

			for _, c := range goldenCases(dates, reportID) {
				t.Run(c.name, func(t *testing.T) {
					if c.building {
						if _, err := (&facts.JobStore{DB: database, Dialect: kdb.Resolve(tc.dialect)}).ReplaceWhole(context.Background(), "Sale"); err != nil {
							t.Fatalf("queue sales rebuild: %v", err)
						}
						defer drainGoldenRebuild(context.Background(), t, database, tc.dialect, reg)
					}
					thread := goldenThreadBase + strings.ReplaceAll(c.name, " ", "-")
					for _, turn := range c.turns {
						model.script(turn.steps)
						before := model.served()
						got := askZana(t, router, c.user, c.roles, thread, turn.message)
						if model.served()-before != len(turn.steps) {
							t.Fatalf("model answered %d times, want %d", model.served()-before, len(turn.steps))
						}
						turn.check(t, got, model.since(before))
					}
				})
			}
		})
	}
}
