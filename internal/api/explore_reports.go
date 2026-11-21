package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// Visibility values of a saved report, and the longest name it may have.
const (
	reportShop    = "shop"
	reportPrivate = "private"
	maxReportName = 120
)

// reportsAPI serves the saved Explore reports of the request's site under /explore/reports. The administrator
// role saves and changes shop reports; a caller who can explore saves private ones, which only they see.
type reportsAPI struct {
	metrics metricsAPI
}

// mount binds the report routes under group.
func (r reportsAPI) mount(group *gin.RouterGroup) {
	group.GET("", r.list)
	group.POST("", r.create)
	group.GET("/:id", r.get)
	group.PUT("/:id", r.update)
	group.DELETE("/:id", r.remove)
	group.POST("/:id/copy", r.copy)
}

// store returns the report store of the request's site, or writes the refusal when the site has no database.
func (r reportsAPI) store(c *gin.Context) (reportStore, bool) {
	database := getSiteDB(c, r.metrics.siteDB)
	if database == nil {
		writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Saved reports not available for this site", nil)
		return reportStore{}, false
	}
	return reportStore{database: database, dialect: siteDialectFromContext(c, r.metrics.dialect)}, true
}

// callerID is the verified user the request runs as, empty when there is none.
func callerID(c *gin.Context) string {
	return actorOf(c).SubjectUserID
}

// isOwner reports whether the caller saved rep.
func isOwner(c *gin.Context, rep savedReport) bool {
	id := callerID(c)
	return id != "" && id == rep.Owner
}

// readable reports whether the caller may read every record type of a report.
func (r reportsAPI) readable(c *gin.Context, models []string) bool {
	_, denial := metricScope(c, analyticsRegistry(c, r.metrics.registry), models)
	return denial == nil
}

// visible reports whether the caller may see rep: a shop report, or the caller's own private one, whose record
// types the caller can all read.
func (r reportsAPI) visible(c *gin.Context, rep savedReport) bool {
	if rep.Visibility == reportPrivate && !isOwner(c, rep) {
		return false
	}
	return r.readable(c, rep.RecordTypes)
}

// load returns the report named in the path when the caller can see it. Otherwise it writes 404.
func (r reportsAPI) load(c *gin.Context, store reportStore) (savedReport, bool) {
	rep, err := store.get(c.Request.Context(), c.Param("id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !r.visible(c, rep)) {
		notFoundError(c, "reports.not_found", "Saved report not found", nil)
		return savedReport{}, false
	}
	if err != nil {
		internalError(c, "loading saved report", err)
		return savedReport{}, false
	}
	return rep, true
}

// mayChange reports whether the caller may save or delete rep. A shop report needs the administrator role; a
// private one needs its owner. It writes the refusal and reports false otherwise.
func mayChange(c *gin.Context, rep savedReport) bool {
	if rep.Visibility == reportShop {
		return requireAdmin(c)
	}
	if isOwner(c, rep) {
		return true
	}
	writeError(c, http.StatusForbidden, "reports.forbidden", "Only the owner can change this report", nil)
	return false
}

// reportDraft is a report the caller sent, checked against its name rules, the catalog and the caller's grants.
type reportDraft struct {
	name       string
	visibility string
	question   contract.SavedQuestion
	models     []string
}

// checkDraft checks the name, the visibility and the question of a report to save. A shop report needs the
// administrator role; a private one needs an authenticated user. It writes the refusal and reports false.
func (r reportsAPI) checkDraft(c *gin.Context, name, visibility string, question contract.SavedQuestion) (reportDraft, bool) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		writeError(c, http.StatusUnprocessableEntity, "reports.name_required", "Give the report a name.", nil)
		return reportDraft{}, false
	case utf8.RuneCountInString(name) > maxReportName:
		writeError(c, http.StatusUnprocessableEntity, "reports.name_invalid", fmt.Sprintf("A report name is at most %d characters.", maxReportName), nil)
		return reportDraft{}, false
	case visibility != reportShop && visibility != reportPrivate:
		writeError(c, http.StatusUnprocessableEntity, "reports.visibility_invalid", "Visibility is shop or private.", nil)
		return reportDraft{}, false
	case visibility == reportShop && !requireAdmin(c):
		return reportDraft{}, false
	case visibility == reportPrivate && callerID(c) == "":
		writeError(c, http.StatusForbidden, "reports.owner_required", "Private reports need an authenticated user.", nil)
		return reportDraft{}, false
	}
	cat, err := r.metrics.catalog(c)
	if err != nil {
		internalError(c, "loading metric definitions", err)
		return reportDraft{}, false
	}
	join := map[string]bool{}
	if reg := analyticsRegistry(c, r.metrics.registry); reg != nil {
		_, join = exploreAccess(c, reg, reg.All())
	}
	models, err := checkSavedQuestion(cat, join, question)
	if err != nil {
		writeError(c, http.StatusUnprocessableEntity, "reports.question_invalid", err.Error(), nil)
		return reportDraft{}, false
	}
	if !r.readable(c, models) {
		writeError(c, http.StatusForbidden, "reports.forbidden", "You cannot read every record type this report uses.", nil)
		return reportDraft{}, false
	}
	return reportDraft{name: name, visibility: visibility, question: question, models: models}, true
}

// saveError writes 409 when the unique name index refused the row, and 500 for any other failure.
func saveError(c *gin.Context, msg string, err error) {
	if errors.Is(err, errReportNameTaken) {
		conflictError(c, "reports.name_taken", "There's already a report with this name. Pick another name.", nil)
		return
	}
	internalError(c, msg, err)
}

// checkName writes 409 when another report already has the name. It reports false when the name is taken.
func checkName(c *gin.Context, store reportStore, name, exceptID string) bool {
	taken, err := store.nameTaken(c.Request.Context(), name, exceptID)
	if err != nil {
		internalError(c, "checking saved report name", err)
		return false
	}
	if taken {
		conflictError(c, "reports.name_taken", fmt.Sprintf("There's already a report called %q. Pick another name.", name), nil)
		return false
	}
	return true
}

// list answers GET /explore/reports with the reports the caller can see, by name.
func (r reportsAPI) list(c *gin.Context) {
	store, ok := r.store(c)
	if !ok {
		return
	}
	reports, err := store.list(c.Request.Context())
	if err != nil {
		internalError(c, "listing saved reports", err)
		return
	}
	out := []contract.SavedReport{}
	for _, rep := range reports {
		if r.visible(c, rep) {
			out = append(out, rep.contract())
		}
	}
	c.JSON(http.StatusOK, Response{Data: out})
}

// get answers GET /explore/reports/:id.
func (r reportsAPI) get(c *gin.Context) {
	store, ok := r.store(c)
	if !ok {
		return
	}
	rep, ok := r.load(c, store)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, Response{Data: rep.contract()})
}

// create answers POST /explore/reports with a new report owned by the caller.
func (r reportsAPI) create(c *gin.Context) {
	var req contract.SavedReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid saved report", nil)
		return
	}
	store, ok := r.store(c)
	if !ok {
		return
	}
	draft, ok := r.checkDraft(c, req.Name, req.Visibility, req.Question)
	if !ok || !checkName(c, store, draft.name, "") {
		return
	}
	r.save(c, store, savedReport{Name: draft.name, Visibility: draft.visibility, Owner: callerID(c), Question: draft.question, RecordTypes: draft.models})
}

// save inserts a new report and writes it with 201.
func (r reportsAPI) save(c *gin.Context, store reportStore, rep savedReport) {
	now := time.Now().UTC()
	rep.ID, rep.Created, rep.Modified = contract.NewID(), now, now
	if err := store.insert(c.Request.Context(), rep); err != nil {
		saveError(c, "saving report", err)
		return
	}
	c.JSON(http.StatusCreated, Response{Data: rep.contract()})
}

// update answers PUT /explore/reports/:id: a rename, a change of visibility or a new question. A valid
// question clears the broken state.
func (r reportsAPI) update(c *gin.Context) {
	var req contract.SavedReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid saved report", nil)
		return
	}
	store, ok := r.store(c)
	if !ok {
		return
	}
	cur, ok := r.load(c, store)
	if !ok || !mayChange(c, cur) {
		return
	}
	draft, ok := r.checkDraft(c, req.Name, req.Visibility, req.Question)
	if !ok || !checkName(c, store, draft.name, cur.ID) {
		return
	}
	cur.Name, cur.Visibility, cur.Question, cur.RecordTypes = draft.name, draft.visibility, draft.question, draft.models
	cur.BrokenReason, cur.BrokenAt = "", time.Time{}
	cur.Modified = time.Now().UTC()
	if err := store.update(c.Request.Context(), cur); err != nil {
		saveError(c, "updating saved report", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: cur.contract()})
}

// remove answers DELETE /explore/reports/:id.
func (r reportsAPI) remove(c *gin.Context) {
	store, ok := r.store(c)
	if !ok {
		return
	}
	cur, ok := r.load(c, store)
	if !ok || !mayChange(c, cur) {
		return
	}
	if err := store.remove(c.Request.Context(), cur.ID); err != nil {
		internalError(c, "deleting saved report", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// copy answers POST /explore/reports/:id/copy with a new report under a new name, owned by the caller. The
// copy is checked like a new report, so a broken question cannot be copied.
func (r reportsAPI) copy(c *gin.Context) {
	var req contract.SavedReportCopyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid report copy", nil)
		return
	}
	store, ok := r.store(c)
	if !ok {
		return
	}
	src, ok := r.load(c, store)
	if !ok {
		return
	}
	draft, ok := r.checkDraft(c, req.Name, req.Visibility, src.Question)
	if !ok || !checkName(c, store, draft.name, "") {
		return
	}
	r.save(c, store, savedReport{Name: draft.name, Visibility: draft.visibility, Owner: callerID(c), Question: draft.question, RecordTypes: draft.models})
}

// applyActivatedConfig applies an activated snapshot to the registry, then rechecks the site's saved reports
// against it.
func applyActivatedConfig(ctx context.Context, database *sql.DB, dialect db.Dialect, snapshot *doctype.ConfigSnapshot, reg *doctype.Registry) {
	configstore.ApplySnapshotToRegistry(snapshot, reg)
	markBrokenReports(ctx, database, dialect, snapshot, reg)
}

// markBrokenReports rechecks every saved report of a site against the catalog of a newly activated
// configuration and records why a report no longer runs. Activation has already committed, so a failure
// is logged and the reports keep their last state.
func markBrokenReports(ctx context.Context, database *sql.DB, dialect db.Dialect, snapshot *doctype.ConfigSnapshot, reg *doctype.Registry) {
	defs, err := decodeConfigItems[metrics.Metric](snapshot.Metrics, "metric")
	if err != nil {
		slog.Error("decode metrics for saved report check", "error", err)
		return
	}
	cat := metrics.NewCatalog(defs, reg.All())
	join := map[string]bool{}
	for _, dt := range reg.All() {
		join[dt.Name] = true
	}
	store := reportStore{database: database, dialect: dialect}
	reports, err := store.list(ctx)
	if err != nil {
		slog.Error("list saved reports for activation check", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, rep := range reports {
		if err := store.setState(ctx, rep.recheck(cat, join, now)); err != nil {
			slog.Error("record saved report state", "report", rep.ID, "error", err)
		}
	}
}
