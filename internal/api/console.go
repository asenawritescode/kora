package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/auth"
	kdb "github.com/asenawritescode/kora/internal/db"
	sqlDialect "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// onboardRateLimiter tracks IP → count for self-service site creation.
var (
	onboardLimiter                = newOnboardRateLimiter(3)
	ErrTenantProvisioningCapacity = errors.New("tenant provisioning capacity exhausted")
)

type onboardRateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Time
	counts map[string]int
}

func newOnboardRateLimiter(max int) *onboardRateLimiter {
	return &onboardRateLimiter{max: max, counts: make(map[string]int)}
}

func (l *onboardRateLimiter) allow(ip string, now time.Time) bool {
	window := now.UTC().Truncate(time.Hour)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.window.Equal(window) {
		l.window = window
		l.counts = make(map[string]int)
	}
	if l.counts[ip] >= l.max {
		return false
	}
	l.counts[ip]++
	return true
}

// ConsoleHandler holds dependencies for console API endpoints.
type ConsoleHandler struct {
	SystemGuard           *auth.SystemGuard
	PlatformWorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	TenantSiteAdmission func(context.Context, *site.Config) (func(), error)
	SiteRouter          *net.SiteRouter
	ProvisioningStore   *site.OnboardingStore
	AllowOnboarding     bool
	SiteStorages        map[string]storage.Backend
	RuntimeServices     *SiteRuntimeServices
	// PrepareRuntime installs the same sidecars used for directory hot-add before
	// a newly-created site becomes routable.
	PrepareRuntime func(site.DBSiteInfo, *net.LoadedSite) error
	// DiscardRuntime releases any sidecars installed by PrepareRuntime when the
	// router rejects the new site.
	DiscardRuntime func(*net.LoadedSite)
	ResolveStorage func(*site.Config) (storage.Backend, error)
	// ReloadRuntime publishes the durable directory config through the tenant
	// runtime coordinator before this handler reports the update as active.
	ReloadRuntime      func(context.Context, string) error
	queuedJobsMu       sync.Mutex
	queuedJobs         map[string]bool
	jobLifecycleMu     sync.Mutex
	jobCtx             context.Context
	jobCancel          context.CancelFunc
	jobClosing         bool
	jobJoinOnce        sync.Once
	jobDone            chan struct{}
	jobWorkers         sync.WaitGroup
	PlatformDBType     string
	PlatformDBHost     string
	PlatformDBPort     int
	PlatformDBUser     string
	PlatformDBPassword string
	PlatformDB         *sql.DB // Existing platform DB connection (for LibSQL reuse)
}

const maxConcurrentOnboardingJobs = 32

// RequirePlatformWork bounds platform console operations that use the
// administrative database. Authenticate first on protected routes so rejected
// callers do not consume platform work capacity.
func (h *ConsoleHandler) RequirePlatformWork(c *gin.Context) {
	if admitted, _ := c.Get("platform_work_admitted"); admitted == true {
		c.Next()
		return
	}
	if h == nil || h.PlatformWorkAdmission == nil {
		c.Next()
		return
	}
	release, err := h.PlatformWorkAdmission.Acquire(c.Request.Context())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			c.Abort()
			return
		}
		c.Header("Retry-After", "1")
		writeError(c, http.StatusServiceUnavailable, "dependency.unavailable", "Platform work capacity is temporarily exhausted", nil)
		c.Abort()
		return
	}
	var releaseOnce sync.Once
	releaseAdmittedWork := func() { releaseOnce.Do(release) }
	defer releaseAdmittedWork()
	c.Set("platform_work_release", releaseAdmittedWork)
	c.Set("platform_work_admitted", true)
	c.Next()
}

// releasePlatformWork lets a handler end its platform DB unit before entering
// tenant activation, which may use the same endpoint-shared admission lane.
func releasePlatformWork(c *gin.Context) {
	if c == nil {
		return
	}
	if value, ok := c.Get("platform_work_release"); ok {
		if release, ok := value.(func()); ok && release != nil {
			release()
			c.Set("platform_work_admitted", false)
		}
	}
}

func (h *ConsoleHandler) acquirePlatformWork(ctx context.Context) (func(), error) {
	if h == nil || h.PlatformWorkAdmission == nil {
		return func() {}, nil
	}
	return h.PlatformWorkAdmission.Acquire(ctx)
}

func (h *ConsoleHandler) withTenantSiteAdmission(ctx context.Context, config *site.Config, create func() error) error {
	if h == nil || ctx == nil || config == nil || create == nil || h.TenantSiteAdmission == nil {
		return errors.New("site provisioning requires tenant work admission")
	}
	release, err := h.TenantSiteAdmission(ctx, config)
	if err != nil {
		return err
	}
	defer release()
	return create()
}

func (h *ConsoleHandler) withTenantSiteAdmissionRetry(ctx context.Context, config *site.Config, create func() error) error {
	if ctx == nil {
		return errors.New("site provisioning retry requires context")
	}
	for {
		err := h.withTenantSiteAdmission(ctx, config, create)
		if err == nil || !errors.Is(err, ErrTenantProvisioningCapacity) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// NewConsoleHandler creates a console API handler.
func NewConsoleHandler(guard *auth.SystemGuard, sr *net.SiteRouter, dbType, dbHost, dbUser, dbPassword string, dbPort int, platformDB *sql.DB, allowOnboarding bool) *ConsoleHandler {
	return &ConsoleHandler{
		SystemGuard:        guard,
		SiteRouter:         sr,
		ProvisioningStore:  site.NewOnboardingStore(platformDB, dbType),
		AllowOnboarding:    allowOnboarding,
		queuedJobs:         make(map[string]bool),
		PlatformDBType:     dbType,
		PlatformDBHost:     dbHost,
		PlatformDBPort:     dbPort,
		PlatformDBUser:     dbUser,
		PlatformDBPassword: dbPassword,
		PlatformDB:         platformDB,
		SiteStorages:       nil,
	}
}

// Start initializes the durable provisioning store. Rate limiting uses a
// fixed-window counter and does not require a background sweeper.
func (h *ConsoleHandler) Start() {
	h.jobLifecycleMu.Lock()
	if h.jobCtx == nil && !h.jobClosing {
		h.jobCtx, h.jobCancel = context.WithCancel(context.Background())
	}
	h.jobLifecycleMu.Unlock()
	if h.ProvisioningStore != nil {
		if err := h.ProvisioningStore.Bootstrap(); err != nil {
			slog.Error("provisioning store bootstrap failed", "error", err)
		}
	}
}

func (h *ConsoleHandler) scheduleOnboard(job site.OnboardingJob, req onboardRequest) bool {
	h.jobLifecycleMu.Lock()
	if h.jobClosing {
		h.jobLifecycleMu.Unlock()
		return false
	}
	if h.jobCtx == nil {
		h.jobCtx, h.jobCancel = context.WithCancel(context.Background())
	}
	jobCtx := h.jobCtx
	h.queuedJobsMu.Lock()
	if h.queuedJobs[job.ID] {
		h.queuedJobsMu.Unlock()
		h.jobLifecycleMu.Unlock()
		return true
	}
	if h.queuedJobs == nil {
		h.queuedJobs = make(map[string]bool)
	}
	if len(h.queuedJobs) >= maxConcurrentOnboardingJobs {
		h.queuedJobsMu.Unlock()
		h.jobLifecycleMu.Unlock()
		return false
	}
	h.queuedJobs[job.ID] = true
	h.jobWorkers.Add(1)
	h.queuedJobsMu.Unlock()
	h.jobLifecycleMu.Unlock()

	go func() {
		defer h.jobWorkers.Done()
		defer func() {
			h.queuedJobsMu.Lock()
			delete(h.queuedJobs, job.ID)
			h.queuedJobsMu.Unlock()
		}()
		h.processOnboardJob(jobCtx, job, req)
	}()
	return true
}

// CloseOnboardingJobs cancels provisioning jobs and joins them before the
// process closes the platform database they use.
func (h *ConsoleHandler) CloseOnboardingJobs(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.jobLifecycleMu.Lock()
	h.jobClosing = true
	if h.jobCancel != nil {
		h.jobCancel()
	}
	if h.jobDone == nil {
		h.jobDone = make(chan struct{})
	}
	done := h.jobDone
	h.jobJoinOnce.Do(func() {
		go func() {
			h.jobWorkers.Wait()
			close(done)
		}()
	})
	h.jobLifecycleMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("join onboarding provisioning jobs: %w", ctx.Err())
	}
}

type onboardRequest struct {
	Hostname      string `json:"hostname"`
	AdminEmail    string `json:"admin_email"`
	AdminPassword string `json:"admin_password"`
	AdminFullName string `json:"admin_full_name"`
	PlatformType  string `json:"platform_type"`
	PlatformHost  string `json:"platform_host"`
	PlatformPort  int    `json:"platform_port"`
	PlatformUser  string `json:"platform_user"`
	PlatformPass  string `json:"platform_password"`
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

// HandleLogin authenticates a console super-admin.
// POST /api/console/login
func (h *ConsoleHandler) HandleLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", nil)
		return
	}

	valid, needsChange := h.SystemGuard.ValidateWithChangeCheck(req.Email, req.Password)
	if !valid {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: ErrorBody{
			Code:    "auth.invalid_credentials",
			Message: "Invalid credentials",
		}})
		return
	}

	token, err := h.SystemGuard.CreateSessionBounded(req.Email)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "resource.capacity_exhausted", "Console session capacity is temporarily exhausted", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: consoleLoginResponse{
		Token:               token,
		Email:               req.Email,
		NeedsPasswordChange: needsChange,
	}})
}

// HandleChangePassword forces a password change (required on first login with default creds).
// POST /api/console/change-password
func (h *ConsoleHandler) HandleChangePassword(c *gin.Context) {
	token := c.GetHeader("Authorization")
	token = strings.TrimPrefix(token, "Bearer ")

	if !h.SystemGuard.ValidateSessionBool(token) {
		writeError(c, http.StatusUnauthorized, "auth.session_invalid", "Invalid session", nil)
		return
	}

	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.NewPassword == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "New password required", map[string]any{"field": "new_password"})
		return
	}

	if err := h.SystemGuard.UpdatePassword(req.NewPassword); err != nil {
		slog.Error("console password change failed", "error", err)
		writeError(c, http.StatusInternalServerError, "internal.password_update_failed", "Password could not be changed", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: consoleMessageResponse{Message: "Password changed"}})
}

// ---------------------------------------------------------------------------
// Auth middleware for console API routes
// ---------------------------------------------------------------------------

// RequireConsoleAuth is middleware that validates the console session.
// Accepts Authorization: Bearer <token> header OR kora_console_sid cookie.
func (h *ConsoleHandler) RequireConsoleAuth(c *gin.Context) {
	token := c.GetHeader("Authorization")
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		// Fallback: check the console session cookie.
		if sid, err := c.Cookie("kora_console_sid"); err == nil && sid != "" {
			token = sid
		}
	}
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: ErrorBody{
			Code:    "auth.authentication_required",
			Message: "Authentication required",
		}})
		return
	}
	if !h.SystemGuard.ValidateSessionBool(token) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: ErrorBody{
			Code:    "auth.session_invalid",
			Message: "Invalid or expired session",
		}})
		return
	}
	c.Next()
}

// ---------------------------------------------------------------------------
// Site Management
// ---------------------------------------------------------------------------

// HandleListSites returns all loaded sites with status.
// Falls back to querying the database directly when no sites are loaded (e.g. after
// container redeploy where site_config.yaml files were lost but DB data persists).
// GET /api/console/sites
func (h *ConsoleHandler) HandleListSites(c *gin.Context) {
	var sites []*net.RuntimeSnapshot
	if h.SiteRouter != nil {
		sites = h.SiteRouter.AllSnapshots()
	}
	managedRuntimes := len(sites) > 0

	// If no sites in memory, try the database — sites survive redeploys there.
	if len(sites) == 0 && h.PlatformDB != nil {
		release, admissionErr := h.acquirePlatformWork(c.Request.Context())
		if admissionErr != nil {
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "dependency.unavailable", "Platform work capacity is temporarily exhausted", nil)
			return
		}
		dbSites, discoverErr := site.DiscoverSitesFromDB(context.Background(), h.PlatformDB)
		release()
		if discoverErr == nil {
			for _, info := range dbSites {
				snapshot := (&net.LoadedSite{
					SiteID: info.SiteID, ConfigRevision: info.ConfigRevision, Status: info.Status, DBType: info.DBType,
					Name: info.Name,
					Config: net.SiteRouterConfig{
						Hostname:      info.Name,
						Domains:       info.Domains,
						FileStorage:   info.FileStorage,
						StorageBucket: info.StorageBucket,
					},
					DB: h.PlatformDB,
				}).Snapshot()
				sites = append(sites, snapshot)
			}
		}
	}

	type SiteEntry struct {
		SiteID         string   `json:"site_id"`
		ConfigRevision uint64   `json:"config_revision"`
		Name           string   `json:"name"`
		Domains        []string `json:"domains"`
		FileStorage    string   `json:"file_storage"`
		StorageBucket  string   `json:"storage_bucket,omitempty"`
		DocTypes       int      `json:"doctypes"`
		Status         string   `json:"status"`
	}
	var result []SiteEntry
	for _, s := range sites {
		status := s.Status
		if status == "" {
			status = "active"
		}
		if status == "active" || status == "degraded" {
			var dbToPing *sql.DB
			var release func()
			var releaseWork func()
			var pingErr error
			admissionRejected := false
			if managedRuntimes && h.SiteRouter != nil {
				handle, err := h.SiteRouter.AcquireByID(c.Request.Context(), string(s.Activation.Tenant))
				if err != nil {
					status = "unavailable"
				} else {
					if current := handle.Snapshot(); current != nil {
						s = current
						dbToPing = current.DB
						if current.HasServices && current.Services.WorkAdmission != nil {
							releaseWork, pingErr = current.Services.WorkAdmission.Acquire(c.Request.Context())
							admissionRejected = pingErr != nil
						}
					}
					release = handle.Release
				}
			} else {
				dbToPing = s.DB
				releaseWork, pingErr = h.acquirePlatformWork(c.Request.Context())
				admissionRejected = pingErr != nil
			}
			if pingErr != nil {
				status = "unavailable"
			}
			if dbToPing == nil {
				if status == "active" || status == "degraded" {
					status = "unknown"
				}
			} else if pingErr == nil {
				pingErr = dbToPing.PingContext(c.Request.Context())
			}
			if releaseWork != nil {
				releaseWork()
			}
			if release != nil {
				release()
			}
			if pingErr != nil && !admissionRejected {
				status = "error"
			}
		}
		docTypes := 0
		if s.Registry != nil {
			docTypes = len(s.Registry.All())
		}
		result = append(result, SiteEntry{
			SiteID:         string(s.Activation.Tenant),
			ConfigRevision: s.ConfigRevision,
			Name:           s.Name,
			Domains:        s.Config.Domains,
			FileStorage:    s.Config.FileStorage,
			StorageBucket:  s.Config.StorageBucket,
			DocTypes:       docTypes,
			Status:         status,
		})
	}
	if result == nil {
		result = []SiteEntry{} // return empty array, not null
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

// HandleCreateSite creates a new site: database, config, bootstrap, admin user.
// POST /api/console/sites
// Only hostname, admin_email, and admin_password are required.
// DB fields are optional — platform defaults from env vars are used when empty.
func (h *ConsoleHandler) HandleCreateSite(c *gin.Context) {
	var req struct {
		Hostname      string `json:"hostname"`
		DBType        string `json:"db_type"`
		DBHost        string `json:"db_host"`
		DBPort        int    `json:"db_port"`
		DBName        string `json:"db_name"`
		DBUser        string `json:"db_user"`
		DBPassword    string `json:"db_password"`
		Domains       string `json:"domains"` // comma-separated extra domains
		AdminEmail    string `json:"admin_email"`
		AdminPassword string `json:"admin_password"`
		AdminFullName string `json:"admin_full_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", map[string]any{"error": err.Error()})
		return
	}
	if req.Hostname == "" || req.AdminEmail == "" || req.AdminPassword == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "hostname, admin_email, and admin_password are required", map[string]any{"fields": []string{"hostname", "admin_email", "admin_password"}})
		return
	}
	if h.SiteRouter != nil && h.SiteRouter.SnapshotByName(req.Hostname) != nil {
		writeError(c, http.StatusConflict, "site.already_exists", "A site with this hostname is already active", nil)
		return
	}

	slog.Info("creating site via console", "hostname", req.Hostname)

	// Resolve platform DB credentials: handler fields (from common config) → env vars.
	platformType := h.PlatformDBType
	if platformType == "" {
		platformType = os.Getenv("KORA_DB_TYPE")
	}
	if platformType == "" {
		platformType = "mysql"
	}
	platformHost := h.PlatformDBHost
	if platformHost == "" {
		platformHost = os.Getenv("KORA_DB_HOST")
	}
	platformPort := h.PlatformDBPort
	if platformPort == 0 {
		platformPort = envConsoleInt("KORA_DB_PORT")
	}
	platformUser := h.PlatformDBUser
	if platformUser == "" {
		platformUser = os.Getenv("KORA_DB_USER")
	}
	platformPass := h.PlatformDBPassword
	if platformPass == "" {
		platformPass = os.Getenv("KORA_DB_PASSWORD")
	}

	// Parse comma-separated extra domains.
	var extraDomains []string
	if req.Domains != "" {
		for _, d := range strings.Split(req.Domains, ",") {
			d = strings.TrimSpace(d)
			if d != "" && d != req.Hostname {
				extraDomains = append(extraDomains, d)
			}
		}
	}

	createInput := site.CreateSiteInput{
		Hostname:           req.Hostname,
		DBType:             req.DBType,
		DBHost:             req.DBHost,
		DBPort:             req.DBPort,
		DBName:             req.DBName,
		DBUser:             req.DBUser,
		DBPassword:         req.DBPassword,
		AdminEmail:         req.AdminEmail,
		AdminFullName:      req.AdminFullName,
		ExtraDomains:       extraDomains,
		PlatformDBType:     platformType,
		PlatformDBHost:     platformHost,
		PlatformDBPort:     platformPort,
		PlatformDBUser:     platformUser,
		PlatformDBPassword: platformPass,
		PlatformDBDSN:      os.Getenv("DB_DSN"),
		PlatformDB:         h.PlatformDB,
	}
	var result *site.CreateSiteResult
	err := h.withTenantSiteAdmission(c.Request.Context(), createInput.ResolvedTenantConfig(), func() error {
		var createErr error
		result, createErr = site.CreateSite(context.Background(), createInput)
		return createErr
	})
	if err != nil {
		if errors.Is(err, ErrTenantProvisioningCapacity) {
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "dependency.unavailable", "Tenant provisioning capacity is temporarily exhausted", nil)
			return
		}
		slog.Error("creating site failed", "hostname", req.Hostname, "error", err)
		errMsg := err.Error()
		// Map known errors to user-friendly messages.
		switch {
		case strings.Contains(errMsg, "connection refused"):
			errMsg = "Cannot connect to MySQL server. Is MySQL running?"
		case strings.Contains(errMsg, "Access denied"):
			errMsg = "Invalid database credentials. Check your DB user and password."
		case strings.Contains(errMsg, "Unknown database"):
			errMsg = "Cannot access the database server. Check your DB host and port."
		default:
			errMsg = "Failed to create site: " + errMsg
		}
		writeError(c, http.StatusInternalServerError, "site.create_failed", errMsg, nil)
		return
	}

	// Hot-add site to the running router.
	// Keep routing domains explicit. Do not auto-add the current request host here:
	// path-based access should work without pretending the app host is a tenant domain.
	domains := []string{req.Hostname}
	domains = append(domains, extraDomains...)
	loaded := &net.LoadedSite{
		SiteID: result.SiteID,
		DBType: result.Config.DBType,
		Name:   req.Hostname,
		Config: net.SiteRouterConfig{
			Hostname:      req.Hostname,
			Domains:       domains,
			FileStorage:   result.Config.FileStorage,
			StorageBucket: result.Config.StorageBucket,
		},
		DB: result.DB, Registry: result.Registry, SessionManager: h.newCreatedSessionManager(result.DB, result.Config.DBType),
	}
	published := false
	defer func() {
		if !published {
			_ = loaded.SessionManager.Close()
			_ = loaded.DB.Close()
		}
	}()
	releasePlatformWork(c)
	if err := h.prepareCreatedRuntime(c.Request.Context(), loaded, result.SiteID, result.Config); err != nil {
		_ = loaded.SessionManager.Close()
		_ = loaded.DB.Close()
		writeError(c, http.StatusInternalServerError, "site.registry_read_failed", "Site was created but could not be loaded into the site directory", nil)
		return
	}
	if h.ResolveStorage != nil && h.PrepareRuntime == nil {
		st, err := h.ResolveStorage(result.Config)
		if err != nil {
			slog.Error("provisioning storage failed", "hostname", req.Hostname, "error", err)
			writeError(c, http.StatusInternalServerError, "storage.provision_failed", "Site created, but storage provisioning failed", nil)
			return
		}
		if h.RuntimeServices != nil {
			h.RuntimeServices.SetStorage(req.Hostname, st)
		}
		if h.SiteStorages != nil && h.RuntimeServices == nil {
			h.SiteStorages[req.Hostname] = st
		}
	}
	if err := h.SiteRouter.AddSite(loaded); err != nil {
		if h.DiscardRuntime != nil {
			h.DiscardRuntime(loaded)
		}
		if loaded.DB != nil {
			_ = loaded.DB.Close()
		}
		slog.Error("site runtime registration failed", "hostname", req.Hostname, "error", err)
		writeError(c, http.StatusConflict, "site.alias_conflict", "Site was created but could not be safely routed because an alias or site ID is already assigned", nil)
		return
	}
	published = true

	slog.Info("site created via console", "hostname", req.Hostname, "db_name", result.Config.DBName)
	c.JSON(http.StatusCreated, Response{Data: consoleSiteCreateResponse{
		Hostname: req.Hostname,
		DBName:   result.Config.DBName,
		Status:   "active",
		Admin:    req.AdminEmail,
	}})
}

// HandleOnboard creates a site via public self-service (no console auth).
// POST /api/console/sites/onboard
// Rate limited: 3 per hour per IP.
func (h *ConsoleHandler) HandleOnboard(c *gin.Context) {
	if !h.AllowOnboarding {
		writeError(c, http.StatusNotFound, "feature.disabled", "Public onboarding is disabled", nil)
		return
	}
	var req onboardRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", map[string]any{"error": err.Error()})
		return
	}
	if req.Hostname == "" || req.AdminEmail == "" || req.AdminPassword == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "hostname, admin_email, and admin_password are required", map[string]any{"fields": []string{"hostname", "admin_email", "admin_password"}})
		return
	}
	if len(req.AdminPassword) < 8 {
		writeError(c, http.StatusBadRequest, "validation.password_too_short", "Password must be at least 8 characters", nil)
		return
	}

	// Rate limit by client IP.
	ip := c.ClientIP()
	if !onboardLimiter.allow(ip, time.Now()) {
		writeError(c, http.StatusTooManyRequests, "rate_limit.exceeded", "Too many requests. Please try again later.", nil)
		return
	}

	// Check if hostname is already taken.
	var existing bool
	if h.PlatformDB != nil {
		registry := site.NewSQLSiteRegistry(h.PlatformDB, h.PlatformDBType)
		_, err := registry.ResolveAlias(req.Hostname)
		switch {
		case err == nil:
			existing = true
		case errors.Is(err, sql.ErrNoRows):
			// No canonical site currently owns this hostname; provisioning may proceed.
		case err != nil:
			internalError(c, "checking site availability", err)
			return
		}
	}
	if existing {
		writeError(c, http.StatusConflict, "site.already_exists", "This site name is already taken. Try another.", nil)
		return
	}

	opID := "onboard:" + req.Hostname
	idempotencyKey := req.Hostname + ":" + req.AdminEmail
	fingerprint := req.Hostname + "|" + req.AdminEmail

	if h.ProvisioningStore != nil {
		if err := h.ProvisioningStore.Bootstrap(); err != nil {
			slog.Error("provisioning store bootstrap failed", "error", err)
			writeError(c, http.StatusInternalServerError, "server.store_unavailable", "Failed to initialize provisioning store", nil)
			return
		}
		if existingJob, err := h.ProvisioningStore.LoadJob(req.Hostname, req.Hostname, opID, idempotencyKey, fingerprint); err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Error("loading provisioning job failed", "hostname", req.Hostname, "error", err)
			writeError(c, http.StatusInternalServerError, "provisioning.load_failed", "Failed to check provisioning state", nil)
			return
		} else if existingJob != nil {
			if existingJob.State == site.OnboardingActive {
				c.JSON(http.StatusCreated, Response{Data: consoleSiteCreateResponse{
					Hostname:     req.Hostname,
					WorkspaceURL: "/s/" + req.Hostname + "/workspace",
					AdminEmail:   req.AdminEmail,
					Status:       "active",
				}})
				return
			}
		}
	}

	job := site.OnboardingJob{
		ID:               "prov-" + req.Hostname,
		Site:             req.Hostname,
		Resource:         req.Hostname,
		State:            site.OnboardingRequested,
		OperationID:      opID,
		IdempotencyKey:   idempotencyKey,
		InputFingerprint: fingerprint,
		Attempt:          1,
		CreatedAt:        time.Now().UTC(),
	}
	if h.ProvisioningStore != nil {
		if err := h.persistOnboarding(job, "requested"); err != nil {
			slog.Error("persisting requested provisioning state failed", "job_id", job.ID, "error", err)
			writeError(c, http.StatusInternalServerError, "provisioning.persist_failed", "Failed to save provisioning request", nil)
			return
		}
	}

	if !h.scheduleOnboard(job, req) {
		job.State = site.OnboardingFailed
		job.LastError = "provisioning worker capacity is exhausted or shutting down"
		job.UpdatedAt = time.Now().UTC()
		if err := h.persistOnboarding(job, ""); err != nil {
			slog.Error("persisting rejected provisioning state failed", "job_id", job.ID, "error", err)
		}
		writeError(c, http.StatusServiceUnavailable, "provisioning.capacity_exhausted", "Provisioning capacity is temporarily exhausted", nil)
		return
	}
	c.JSON(http.StatusAccepted, Response{Data: map[string]any{
		"job_id":        job.ID,
		"hostname":      req.Hostname,
		"admin_email":   req.AdminEmail,
		"status":        "requested",
		"workspace_url": "/s/" + req.Hostname + "/workspace",
	}})
}

func (h *ConsoleHandler) persistOnboarding(job site.OnboardingJob, stage string) error {
	if h.ProvisioningStore == nil {
		return nil
	}
	if err := h.ProvisioningStore.UpsertJob(job); err != nil {
		return fmt.Errorf("save %s job state: %w", stage, err)
	}
	if stage != "" {
		if err := h.ProvisioningStore.UpsertCheckpoint(site.OnboardingCheckpoint{JobID: job.ID, Stage: stage, Completed: true, RecordedAt: time.Now().UTC()}); err != nil {
			return fmt.Errorf("save %s checkpoint: %w", stage, err)
		}
	}
	return nil
}

// HandleOnboardStatus returns the current state of a provisioning job.
// GET /api/console/sites/onboard/:job_id
func (h *ConsoleHandler) HandleOnboardStatus(c *gin.Context) {
	jobID := c.Param("job_id")
	if jobID == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "job_id required", map[string]any{"field": "job_id"})
		return
	}
	if h.ProvisioningStore == nil {
		writeError(c, http.StatusNotFound, "server.store_unavailable", "Provisioning store unavailable", nil)
		return
	}
	job, err := h.ProvisioningStore.LoadJobByID(jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "provisioning.job_not_found", "Provisioning job not found", nil)
			return
		}
		slog.Error("loading provisioning job failed", "job_id", jobID, "error", err)
		writeError(c, http.StatusInternalServerError, "provisioning.load_failed", "Failed to load provisioning job", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: consoleProvisioningStatusResponse{
		JobID:            job.ID,
		Site:             job.Site,
		State:            string(job.State),
		Attempt:          job.Attempt,
		OperationID:      job.OperationID,
		IdempotencyKey:   job.IdempotencyKey,
		InputFingerprint: job.InputFingerprint,
		OutputID:         job.OutputID,
		LastError:        job.LastError,
		CreatedAt:        job.CreatedAt,
		UpdatedAt:        job.UpdatedAt,
		WorkspaceURL:     "/s/" + job.Site + "/workspace",
	}})
}

// HandleOnboardJobs lists provisioning jobs for the console.
// GET /api/console/sites/onboard
func (h *ConsoleHandler) HandleOnboardJobs(c *gin.Context) {
	if h.ProvisioningStore == nil {
		c.JSON(http.StatusOK, Response{Data: consoleProvisioningListResponse{Jobs: []consoleProvisioningStatusResponse{}}})
		return
	}
	jobs, err := h.ProvisioningStore.ListJobs()
	if err != nil {
		slog.Error("listing provisioning jobs failed", "error", err)
		writeError(c, http.StatusInternalServerError, "provisioning.list_failed", "Failed to list provisioning jobs", nil)
		return
	}
	out := make([]consoleProvisioningStatusResponse, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, consoleProvisioningStatusResponse{
			JobID:            job.ID,
			Site:             job.Site,
			State:            string(job.State),
			Attempt:          job.Attempt,
			OperationID:      job.OperationID,
			IdempotencyKey:   job.IdempotencyKey,
			InputFingerprint: job.InputFingerprint,
			OutputID:         job.OutputID,
			LastError:        job.LastError,
			CreatedAt:        job.CreatedAt,
			UpdatedAt:        job.UpdatedAt,
			WorkspaceURL:     "/s/" + job.Site + "/workspace",
		})
	}
	c.JSON(http.StatusOK, Response{Data: consoleProvisioningListResponse{Jobs: out}})
}

func (h *ConsoleHandler) processOnboardJob(ctx context.Context, job site.OnboardingJob, req onboardRequest) {
	if ctx.Err() != nil {
		return
	}
	var release func()
	for {
		var err error
		release, err = h.acquirePlatformWork(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
	platformHeld := true
	releasePlatform := func() {
		if platformHeld {
			release()
			platformHeld = false
		}
	}
	defer releasePlatform()
	adminFullName := req.AdminFullName
	if adminFullName == "" {
		adminFullName = strings.Split(req.AdminEmail, "@")[0]
	}
	platformType := req.PlatformType
	if platformType == "" {
		platformType = h.PlatformDBType
	}
	if platformType == "" {
		platformType = os.Getenv("KORA_DB_TYPE")
	}
	if platformType == "" {
		platformType = "mysql"
	}
	platformHost := req.PlatformHost
	if platformHost == "" {
		platformHost = h.PlatformDBHost
	}
	if platformHost == "" {
		platformHost = os.Getenv("KORA_DB_HOST")
	}
	platformPort := req.PlatformPort
	if platformPort == 0 {
		platformPort = h.PlatformDBPort
	}
	if platformPort == 0 {
		platformPort = envConsoleInt("KORA_DB_PORT")
	}
	platformUser := req.PlatformUser
	if platformUser == "" {
		platformUser = h.PlatformDBUser
	}
	if platformUser == "" {
		platformUser = os.Getenv("KORA_DB_USER")
	}
	platformPass := req.PlatformPass
	if platformPass == "" {
		platformPass = h.PlatformDBPassword
	}
	if platformPass == "" {
		platformPass = os.Getenv("KORA_DB_PASSWORD")
	}

	job.State = site.OnboardingValidating
	job.Attempt++
	job.UpdatedAt = time.Now().UTC()
	if h.ProvisioningStore != nil {
		if err := h.persistOnboarding(job, "validated"); err != nil {
			slog.Error("persisting validated provisioning state failed", "job_id", job.ID, "error", err)
			return
		}
	}

	createInput := site.CreateSiteInput{
		Hostname:           req.Hostname,
		AdminEmail:         req.AdminEmail,
		AdminFullName:      adminFullName,
		PlatformDBType:     platformType,
		PlatformDBHost:     platformHost,
		PlatformDBPort:     platformPort,
		PlatformDBUser:     platformUser,
		PlatformDBPassword: platformPass,
		PlatformDBDSN:      os.Getenv("DB_DSN"),
		PlatformDB:         h.PlatformDB,
	}
	var result *site.CreateSiteResult
	err := h.withTenantSiteAdmissionRetry(ctx, createInput.ResolvedTenantConfig(), func() error {
		var createErr error
		result, createErr = site.CreateSite(context.Background(), createInput)
		return createErr
	})
	if err != nil {
		job.State = site.OnboardingFailed
		job.LastError = err.Error()
		job.UpdatedAt = time.Now().UTC()
		if h.ProvisioningStore != nil {
			if saveErr := h.persistOnboarding(job, ""); saveErr != nil {
				slog.Error("persisting failed provisioning state failed", "job_id", job.ID, "error", saveErr)
			}
		}
		slog.Error("self-service site creation failed", "hostname", req.Hostname, "error", err)
		return
	}

	job.State = site.OnboardingProvisioning
	job.OutputID = result.Config.DBName
	job.UpdatedAt = time.Now().UTC()
	if h.ProvisioningStore != nil {
		if err := h.persistOnboarding(job, "provisioned"); err != nil {
			slog.Error("persisting provisioned state failed", "job_id", job.ID, "error", err)
			return
		}
	}

	domains := []string{req.Hostname}
	loaded := &net.LoadedSite{
		SiteID: result.SiteID,
		DBType: result.Config.DBType,
		Name:   req.Hostname,
		Config: net.SiteRouterConfig{
			Hostname:      req.Hostname,
			Domains:       domains,
			FileStorage:   result.Config.FileStorage,
			StorageBucket: result.Config.StorageBucket,
		},
		DB: result.DB, Registry: result.Registry, SessionManager: h.newCreatedSessionManager(result.DB, result.Config.DBType),
	}
	published := false
	defer func() {
		if !published {
			_ = loaded.SessionManager.Close()
			_ = loaded.DB.Close()
		}
	}()
	releasePlatform()
	if err := h.prepareCreatedRuntime(ctx, loaded, result.SiteID, result.Config); err != nil {
		_ = loaded.SessionManager.Close()
		_ = loaded.DB.Close()
		job.State = site.OnboardingFailed
		job.LastError = "site directory lookup failed after provisioning"
		job.UpdatedAt = time.Now().UTC()
		if h.ProvisioningStore != nil {
			if saveErr := h.persistOnboardingAdmitted(ctx, job, ""); saveErr != nil {
				slog.Error("persisting failed provisioning state failed", "job_id", job.ID, "error", saveErr)
			}
		}
		return
	}
	if h.ResolveStorage != nil && h.PrepareRuntime == nil {
		st, err := h.ResolveStorage(result.Config)
		if err != nil {
			slog.Error("provisioning storage failed", "hostname", req.Hostname, "error", err)
			job.State = site.OnboardingFailed
			job.LastError = err.Error()
			job.UpdatedAt = time.Now().UTC()
			if h.ProvisioningStore != nil {
				if saveErr := h.persistOnboardingAdmitted(ctx, job, ""); saveErr != nil {
					slog.Error("persisting failed provisioning state failed", "job_id", job.ID, "error", saveErr)
				}
			}
			return
		}
		if h.RuntimeServices != nil {
			h.RuntimeServices.SetStorage(req.Hostname, st)
		}
		if h.SiteStorages != nil && h.RuntimeServices == nil {
			h.SiteStorages[req.Hostname] = st
		}
	}
	// Persist the active state before publishing the runtime. If the durable write
	// fails, callers can safely retry/recover without a tenant that is live but
	// still recorded as provisioning. The router publication itself is atomic.
	job.State = site.OnboardingActive
	job.UpdatedAt = time.Now().UTC()
	if h.ProvisioningStore != nil {
		if err := h.persistOnboardingAdmitted(ctx, job, "active"); err != nil {
			if h.DiscardRuntime != nil {
				h.DiscardRuntime(loaded)
			}
			if loaded.DB != nil {
				_ = loaded.DB.Close()
			}
			slog.Error("persisting active provisioning state failed", "job_id", job.ID, "error", err)
			return
		}
	}

	if err := h.SiteRouter.AddSite(loaded); err != nil {
		if h.DiscardRuntime != nil {
			h.DiscardRuntime(loaded)
		}
		if loaded.DB != nil {
			_ = loaded.DB.Close()
		}
		job.State = site.OnboardingFailed
		job.LastError = err.Error()
		job.UpdatedAt = time.Now().UTC()
		if h.ProvisioningStore != nil {
			if saveErr := h.persistOnboardingAdmitted(ctx, job, ""); saveErr != nil {
				slog.Error("persisting failed provisioning state failed", "job_id", job.ID, "error", saveErr)
			}
		}
		return
	}
	published = true
	slog.Info("site created via self-service onboarding", "hostname", req.Hostname)
}

// Engine installs tenant work admission before its runtime workers start. Keep
// the new tenant's session sweeper paused until that installer owns startup;
// standalone ConsoleHandler callers retain the historical self-starting path.
func (h *ConsoleHandler) newCreatedSessionManager(database *sql.DB, dbType string) *auth.SessionManager {
	manager := auth.NewOwnedSessionManagerPaused(database).SetDialect(kdb.Resolve(dbType))
	if h == nil || h.PrepareRuntime == nil {
		manager.StartCleanup()
	}
	return manager
}

func (h *ConsoleHandler) prepareCreatedRuntime(ctx context.Context, loaded *net.LoadedSite, siteID string, cfg *site.Config) error { //nolint:contextcheck // tolerates a nil ctx from legacy callers
	if ctx == nil {
		ctx = context.Background()
	}
	var release func()
	if h.PlatformDB != nil {
		var err error
		release, err = h.acquirePlatformWork(ctx)
		if err != nil {
			return fmt.Errorf("admit created-site directory lookup: %w", err)
		}
	} else {
		release = func() {}
	}
	info, err := h.createdSiteInfo(ctx, siteID, cfg)
	release()
	if err != nil {
		return err
	}
	loaded.ConfigRevision = info.ConfigRevision
	loaded.Status = info.Status
	if h.PrepareRuntime != nil {
		if err := h.PrepareRuntime(info, loaded); err != nil {
			if h.DiscardRuntime != nil {
				h.DiscardRuntime(loaded)
			}
			return fmt.Errorf("prepare created runtime: %w", err)
		}
	}
	return nil
}

func (h *ConsoleHandler) persistOnboardingAdmitted(ctx context.Context, job site.OnboardingJob, stage string) error {
	if h == nil || h.ProvisioningStore == nil {
		return nil
	}
	release, err := h.acquirePlatformWork(ctx)
	if err != nil {
		return fmt.Errorf("admit provisioning state write: %w", err)
	}
	defer release()
	return h.persistOnboarding(job, stage)
}

func (h *ConsoleHandler) createdSiteInfo(ctx context.Context, siteID string, cfg *site.Config) (site.DBSiteInfo, error) {
	if h.PlatformDB != nil {
		return site.NewSQLSiteRegistry(h.PlatformDB, h.PlatformDBType).GetByIDContext(ctx, siteID)
	}
	if cfg == nil {
		return site.DBSiteInfo{}, errors.New("created site configuration is missing")
	}
	return site.DBSiteInfo{
		SiteID: siteID, Name: cfg.Hostname, Domains: cfg.Domains(), Status: "active", ConfigRevision: 1,
		DBType: cfg.DBType, DBHost: cfg.DBHost, DBPort: cfg.DBPort, DBName: cfg.DBName,
		DBUser: cfg.DBUser, DBPassword: cfg.DBPassword, FileStorage: cfg.FileStorage, StorageBucket: cfg.StorageBucket,
	}, nil
}

// HandleUpdateSite updates site metadata and file storage settings.
// PUT /api/console/sites/:name
func (h *ConsoleHandler) HandleUpdateSite(c *gin.Context) {
	siteName := c.Param("name")
	if siteName == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "site name required", map[string]any{"field": "name"})
		return
	}

	var req struct {
		Domains       *[]string `json:"domains"`
		FileStorage   *string   `json:"file_storage"`
		StorageBucket *string   `json:"storage_bucket"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", nil)
		return
	}

	_, runtimeHandle, admitted := h.SiteRouter.AcquireByName(c.Request.Context(), siteName)
	if !admitted || runtimeHandle == nil {
		writeError(c, http.StatusNotFound, "site.not_found", "Site not found", map[string]any{"name": siteName})
		return
	}
	defer runtimeHandle.Release()
	runtimeSnapshot := runtimeHandle.Snapshot()
	if runtimeSnapshot == nil {
		writeError(c, http.StatusNotFound, "site.not_found", "Site not found", map[string]any{"name": siteName})
		return
	}
	if h.PlatformDB == nil {
		writeError(c, http.StatusServiceUnavailable, "site.directory_unavailable", "Site directory is unavailable; changes were not saved", nil)
		return
	}
	if h.ReloadRuntime == nil {
		writeError(c, http.StatusServiceUnavailable, "site.runtime_reload_unavailable", "Site runtime refresh is unavailable; changes were not saved", nil)
		return
	}

	newCfg := runtimeSnapshot.Config
	if req.Domains != nil {
		newCfg.Domains = *req.Domains
	}
	if req.FileStorage != nil {
		newCfg.FileStorage = strings.TrimSpace(*req.FileStorage)
	}
	if req.StorageBucket != nil {
		newCfg.StorageBucket = strings.TrimSpace(*req.StorageBucket)
	}
	newCfg.Hostname = siteName
	if newCfg.FileStorage == "" {
		newCfg.FileStorage = "local"
	}
	if strings.ToLower(newCfg.FileStorage) != "s3" {
		newCfg.StorageBucket = ""
	} else if newCfg.StorageBucket == "" {
		newCfg.StorageBucket = site.BucketNameForSite(siteName)
	}
	// The coordinator replacement drains the current generation. Release this
	// handler's pin first so it cannot wait on itself.
	runtimeHandle.Release()

	if h.ResolveStorage != nil {
		resolvedStorage, err := h.ResolveStorage(&site.Config{
			Hostname:      siteName,
			DomainsList:   newCfg.Domains,
			FileStorage:   newCfg.FileStorage,
			StorageBucket: newCfg.StorageBucket,
		})
		if err != nil {
			if closer, ok := resolvedStorage.(io.Closer); ok {
				if closeErr := closer.Close(); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("close rejected storage backend: %w", closeErr))
				}
			}
			slog.Error("resolving updated storage failed", "hostname", siteName, "error", err)
			writeError(c, http.StatusBadRequest, "storage.provision_failed", "Invalid storage configuration", map[string]any{"error": err.Error()})
			return
		}
		if closer, ok := resolvedStorage.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				slog.Error("closing storage validation backend failed", "hostname", siteName, "error", err)
				writeError(c, http.StatusInternalServerError, "storage.provision_failed", "Could not validate storage configuration", nil)
				return
			}
		}
	}

	if err := site.UpdatePlatformSiteRegistration(h.PlatformDB, h.PlatformDBType, siteName, newCfg.Domains, newCfg.FileStorage, newCfg.StorageBucket); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "site.not_found", "Site not found", map[string]any{"name": siteName})
			return
		}
		slog.Error("updating site registry failed", "hostname", siteName, "error", err)
		writeError(c, http.StatusInternalServerError, "site.update_failed", "Failed to update site", map[string]any{"error": err.Error()})
		return
	}

	// Runtime replacement reads the platform directory and may use the same
	// endpoint-shared admission lane as this transaction.
	releasePlatformWork(c)
	if err := h.ReloadRuntime(c.Request.Context(), siteName); err != nil {
		slog.Error("site config persisted but runtime refresh failed", "hostname", siteName, "error", err)
		writeError(c, http.StatusServiceUnavailable, "site.runtime_refresh_pending", "Site settings were saved, but the active runtime has not refreshed yet", nil)
		return
	}

	slog.Info("site updated via console", "hostname", siteName, "domains", newCfg.Domains, "file_storage", newCfg.FileStorage, "storage_bucket", newCfg.StorageBucket)
	c.JSON(http.StatusOK, Response{Data: consoleSiteUpdateResponse{
		Hostname:      siteName,
		Domains:       newCfg.Domains,
		FileStorage:   newCfg.FileStorage,
		StorageBucket: newCfg.StorageBucket,
	}})
}

// HandleDeleteSite deletes a site and all its data.
// DELETE /api/console/sites/:name
// Requires confirmation: {"confirm": "<hostname>"}
func (h *ConsoleHandler) HandleDeleteSite(c *gin.Context) {
	siteName := c.Param("name")
	if siteName == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "site name required", map[string]any{"field": "name"})
		return
	}

	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", nil)
		return
	}
	if req.Confirm != siteName {
		writeError(c, http.StatusBadRequest, "validation.required_confirmation", "Type the site hostname to confirm deletion.", nil)
		return
	}

	loaded := h.SiteRouter.SnapshotByName(siteName)
	if loaded == nil {
		writeError(c, http.StatusNotFound, "site.not_found", "Site not found", map[string]any{"name": siteName})
		return
	}
	if h.PlatformDB == nil {
		writeError(c, http.StatusServiceUnavailable, "site.directory_unavailable", "Site directory is unavailable; deletion was not started", nil)
		return
	}
	siteID := string(loaded.Activation.Tenant)
	if siteID == "" {
		info, err := site.NewSQLSiteRegistry(h.PlatformDB, h.PlatformDBType).ResolveAlias(siteName)
		if err != nil {
			slog.Error("resolving site identity before deletion failed", "hostname", siteName, "error", err)
			writeError(c, http.StatusServiceUnavailable, "site.directory_unavailable", "Site directory is unavailable; deletion was not started", nil)
			return
		}
		siteID = info.SiteID
	}
	if siteID == "" {
		writeError(c, http.StatusConflict, "site.identity_missing", "Site has no canonical identity; deletion was not started", nil)
		return
	}

	// Persist the deleting state and outbox event before dropping tenant data.
	// If the directory is unavailable, no destructive tenant action is allowed.
	registry := site.NewSQLSiteRegistry(h.PlatformDB, h.PlatformDBType)
	deleteInfo, err := registry.GetByID(siteID)
	if err != nil {
		slog.Error("reading site database coordinates before deletion failed", "hostname", siteName, "site_id", siteID, "error", err)
		writeError(c, http.StatusServiceUnavailable, "site.directory_unavailable", "Site directory is unavailable; deletion was not started", nil)
		return
	}
	descriptor, err := registry.SetStatusAndGetDescriptor(siteID, "deleting")
	if err != nil {
		slog.Error("marking site deleting failed", "hostname", siteName, "site_id", siteID, "error", err)
		writeError(c, http.StatusServiceUnavailable, "site.directory_unavailable", "Site directory is unavailable; deletion was not started", nil)
		return
	}
	if err := h.SiteRouter.ApplySiteDescriptor(descriptor); err != nil {
		slog.Error("applying deleting state to local site router failed", "hostname", siteName, "site_id", siteID, "error", err)
		writeError(c, http.StatusServiceUnavailable, "site.delete_pending", "Site is marked for deletion but local routing could not be updated; retry reconciliation", nil)
		return
	}
	idleCtx, cancelIdle := context.WithTimeout(c.Request.Context(), 30*time.Second)
	if err := h.SiteRouter.WaitForTenantIdle(idleCtx, siteID); err != nil {
		cancelIdle()
		writeError(c, http.StatusServiceUnavailable, "site.delete_pending", "Tenant requests have not drained; deletion was not applied", nil)
		return
	}
	cancelIdle()
	if h.DiscardRuntime != nil {
		// The durable deleting state fences new work. Keep request-visible
		// services and sessions alive until all admitted requests have drained;
		// then stop sidecars before destructive tenant cleanup.
		if current := h.SiteRouter.SiteByID(siteID); current != nil {
			h.DiscardRuntime(current)
		}
	}

	// Use the authoritative per-site database coordinates, not a database name
	// guessed from the ingress hostname. Older rows may lack db_name, so retain
	// the historical derivation only as a fallback.
	dbName := deleteInfo.DBName
	if dbName == "" {
		dbName = strings.ReplaceAll(deleteInfo.Name, ".", "_")
	}
	dbType := strings.TrimSpace(deleteInfo.DBType)
	if dbType == "" {
		dbType = h.PlatformDBType
	}
	dbHost := deleteInfo.DBHost
	if dbHost == "" {
		dbHost = h.PlatformDBHost
	}
	dbPort := deleteInfo.DBPort
	if dbPort == 0 {
		dbPort = h.PlatformDBPort
	}
	dbUser := deleteInfo.DBUser
	if dbUser == "" {
		dbUser = h.PlatformDBUser
	}
	dbPassword := deleteInfo.DBPassword
	if dbPassword == "" {
		dbPassword = h.PlatformDBPassword
	}
	dbConfig := &site.Config{DBType: dbType, DBHost: dbHost, DBPort: dbPort, DBName: dbName, DBUser: dbUser, DBPassword: dbPassword}

	slog.Info("deleting site via console", "hostname", siteName)

	if err := site.DeleteSite(site.DeleteSiteInput{
		DB:             loaded.DB,
		Dialect:        sqlDialect.Resolve(dbType),
		Hostname:       deleteInfo.Name,
		SiteID:         siteID,
		PlatformDB:     h.PlatformDB,
		PlatformDBType: h.PlatformDBType,
		DBType:         dbType,
		DBName:         dbName,
		DBHost:         dbHost,
		DBPort:         dbPort,
		DBUser:         dbUser,
		DBPassword:     dbPassword,
		DBDSN:          dbConfig.DSN(),
	}); err != nil {
		slog.Error("deleting site failed", "hostname", siteName, "error", err)
		writeError(c, http.StatusInternalServerError, "site.delete_pending", "Site is unavailable while deletion cleanup is pending; retry after database recovery", nil)
		return
	}

	// Retire the local runtime only after tenant cleanup and directory removal
	// both succeeded. A failed cleanup leaves the site in deleting state so an
	// administrator can retry without reopening traffic.
	removed, retireErr := h.SiteRouter.RetireUnavailable(c.Request.Context(), siteID)
	if retireErr != nil {
		slog.Error("tenant runtime retirement failed after database deletion", "site_id", siteID, "error", retireErr)
		writeError(c, http.StatusInternalServerError, "site.delete_pending", "Site data was deleted but local runtime cleanup is pending", nil)
		return
	}
	if removed != nil {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 30*time.Second)
		closeErr := removed.Close(closeCtx)
		cancelClose()
		if closeErr != nil {
			slog.Error("deleted tenant runtime cleanup failed", "site_id", siteID, "error", closeErr)
			writeError(c, http.StatusInternalServerError, "site.delete_pending", "Site data was deleted but local runtime cleanup is pending", nil)
			return
		}
		h.SiteRouter.FinishRetirement(removed)
	}

	slog.Info("site deleted via console", "hostname", siteName)
	c.JSON(http.StatusOK, Response{Data: consoleSiteDeleteResponse{
		Hostname: siteName,
		Deleted:  true,
	}})
}

// HandleResetSitePassword resets a site user's password.
// POST /api/console/sites/:name/reset-password
func (h *ConsoleHandler) HandleResetSitePassword(c *gin.Context) {
	siteName := c.Param("name")
	if siteName == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "site name required", map[string]any{"field": "name"})
		return
	}

	var req struct {
		Email       string `json:"email"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", nil)
		return
	}
	if req.Email == "" || req.NewPassword == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "email and new_password are required", map[string]any{"fields": []string{"email", "new_password"}})
		return
	}

	_, runtimeHandle, admitted := h.SiteRouter.AcquireByName(c.Request.Context(), siteName)
	if !admitted || runtimeHandle == nil {
		writeError(c, http.StatusNotFound, "site.not_found", "Site not found", map[string]any{"name": siteName})
		return
	}
	defer runtimeHandle.Release()
	loaded := runtimeHandle.Snapshot()
	if loaded == nil || loaded.DB == nil {
		writeError(c, http.StatusNotFound, "site.not_found", "Site not found", map[string]any{"name": siteName})
		return
	}

	// Hash the new password.
	passwordHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		slog.Error("hashing password failed", "error", err)
		writeError(c, http.StatusInternalServerError, "auth.password_hash_failed", "Failed to hash password.", nil)
		return
	}

	// Update the password and revoke all active sessions atomically, using the
	// tenant database's dialect rather than the platform/default dialect.
	dialect := sqlDialect.Resolve(loaded.DBType)
	tx, err := loaded.DB.BeginTx(c.Request.Context(), nil)
	if err != nil {
		internalError(c, "resetting site password", err)
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(sqlDialect.Rebind(dialect,
		"UPDATE _kora_user SET password_hash = ?, modified = CURRENT_TIMESTAMP WHERE email = ?"),
		passwordHash, req.Email,
	)
	if err != nil {
		slog.Error("resetting site password failed", "site", siteName, "email", req.Email, "error", err)
		writeError(c, http.StatusInternalServerError, "site.password_update_failed", "Failed to update password", map[string]any{"error": err.Error()})
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		internalError(c, "checking reset site password", err)
		return
	}
	if rowsAffected == 0 {
		writeError(c, http.StatusNotFound, "user.not_found", "No user found with email", map[string]any{"email": req.Email})
		return
	}

	revokeQuery := fmt.Sprintf("DELETE FROM _kora_session WHERE %s = (SELECT name FROM _kora_user WHERE email = ?)", dialect.QuoteIdent("user"))
	if _, err := tx.Exec(sqlDialect.Rebind(dialect, revokeQuery), req.Email); err != nil {
		internalError(c, "invalidating reset user sessions", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internalError(c, "committing site password reset", err)
		return
	}

	slog.Info("site user password reset via console", "site", siteName, "email", req.Email)
	c.JSON(http.StatusOK, Response{Data: consoleMessageResponse{Message: "Password reset successfully. All existing sessions have been invalidated."}})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// envConsoleInt reads an integer env var, returning 0 if empty or unparseable.
func envConsoleInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n := 0
	for _, c := range v {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
