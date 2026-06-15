package console

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/asenawritescode/kora/auth"
)

//go:embed templates/*.html
var templateFS embed.FS

var tmpl *template.Template

func init() {
	var err error
	tmpl, err = template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		panic(fmt.Sprintf("failed to parse console templates: %v", err))
	}
}

// SiteInfo holds summary data about a loaded site.
type SiteInfo struct {
	Name      string
	DBName    string
	Domains   []string
	DocTypes  int
	Workflows int
	Status    string // "active", "error"
}

// Handler serves the system console at /console.
type Handler struct {
	guard       *auth.SystemGuard
	sites       []SiteInfo
	startedAt   time.Time
	version     string
}

// NewHandler creates a console handler.
func NewHandler(guard *auth.SystemGuard, sites []SiteInfo, version string) *Handler {
	return &Handler{
		guard:     guard,
		sites:     sites,
		startedAt: time.Now(),
		version:   version,
	}
}

// RegisterRoutes registers console routes.
func (h *Handler) RegisterRoutes(router *gin.Engine) {
	// Login page — public.
	router.GET("/console/login", h.HandleLoginPage)
	router.POST("/console/login", h.HandleLogin)

	// Protected routes.
	console := router.Group("/console")
	console.Use(h.guard.Middleware())
	{
		console.GET("/", h.HandleHome)
		console.GET("/sites", h.HandleSites)
		console.GET("/sites/:name", h.HandleSiteDetail)
		console.GET("/health", h.HandleHealth)
		console.POST("/logout", h.HandleLogout)
	}
}

// pageData is passed to every rendered template.
type pageData struct {
	Title      string
	ActiveMenu string
	Sites      []SiteInfo
	Site       *SiteInfo
	Version    string
	Uptime     string
	GoVersion  string
	NumCPU     int
	NumGoroutine int
	Error      string
	Message    string
	Email      string
}

func (h *Handler) baseData(title, activeMenu string) pageData {
	uptime := time.Since(h.startedAt).Round(time.Second).String()
	return pageData{
		Title:      title,
		ActiveMenu: activeMenu,
		Sites:      h.sites,
		Version:    h.version,
		Uptime:     uptime,
		GoVersion:  runtime.Version(),
		NumCPU:     runtime.NumCPU(),
	}
}

// HandleLoginPage renders the login form.
func (h *Handler) HandleLoginPage(c *gin.Context) {
	// If already authenticated via a valid session cookie, redirect to console.
	if sid, _ := c.Cookie("kora_console_sid"); sid != "" {
		if h.guard.ValidateSession(sid) != "" {
			c.Redirect(http.StatusFound, "/console/")
			return
		}
	}

	data := pageData{Title: "Console Login"}
	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(c.Writer, "login.html", data)
}

// HandleLogin processes the console login form.
func (h *Handler) HandleLogin(c *gin.Context) {
	email := c.PostForm("email")
	password := c.PostForm("password")

	if email == "" || password == "" {
		data := pageData{Title: "Console Login", Error: "Email and password are required."}
		c.Header("Content-Type", "text/html; charset=utf-8")
		tmpl.ExecuteTemplate(c.Writer, "login.html", data)
		return
	}

	// Verify against system credentials.
	if err := h.guard.VerifyCredentials(email, password); err != nil {
		data := pageData{Title: "Console Login", Error: "Invalid credentials."}
		c.Header("Content-Type", "text/html; charset=utf-8")
		tmpl.ExecuteTemplate(c.Writer, "login.html", data)
		return
	}

	// Create a proper server-side session with a cryptographically random ID.
	sid := h.guard.CreateSession(email)
	auth.SetSecureCookie(c, "kora_console_sid", sid, int((24 * time.Hour).Seconds()), "/", true)
	c.Redirect(http.StatusFound, "/console/")
}

// HandleLogout destroys the console session and redirects to login.
func (h *Handler) HandleLogout(c *gin.Context) {
	if sid, _ := c.Cookie("kora_console_sid"); sid != "" {
		h.guard.DeleteSession(sid)
	}
	auth.SetSecureCookie(c, "kora_console_sid", "", -1, "/", true)
	c.Redirect(http.StatusFound, "/console/login")
}

// HandleHome renders the system console dashboard.
func (h *Handler) HandleHome(c *gin.Context) {
	data := h.baseData("Dashboard", "dashboard")
	active := 0
	errCount := 0
	for _, s := range h.sites {
		if s.Status == "active" {
			active++
		} else {
			errCount++
		}
	}
	// Store counts in an accessible way for the template.
	data.NumCPU = active
	data.NumGoroutine = errCount
	data.Email = c.GetString("system_email")

	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(c.Writer, "shell.html", data)
}

// HandleSites lists all sites.
func (h *Handler) HandleSites(c *gin.Context) {
	data := h.baseData("Sites", "sites")
	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(c.Writer, "shell.html", data)
}

// HandleSiteDetail shows a single site's details.
func (h *Handler) HandleSiteDetail(c *gin.Context) {
	name := c.Param("name")
	data := h.baseData("Site: "+name, "sites")

	for _, s := range h.sites {
		if s.Name == name {
			data.Site = &s
			break
		}
	}
	if data.Site == nil {
		c.String(http.StatusNotFound, "Site not found")
		return
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(c.Writer, "shell.html", data)
}

// HandleHealth returns system health information.
func (h *Handler) HandleHealth(c *gin.Context) {
	data := h.baseData("Health", "health")
	data.NumGoroutine = runtime.NumGoroutine()

	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(c.Writer, "shell.html", data)
}
