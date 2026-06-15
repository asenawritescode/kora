// Package workspace provides the per-site workspace UI (config-driven SPA).
package workspace

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/asenawritescode/kora/doctype"
)

//go:embed templates/*.html
var templateFS embed.FS

var tmpl *template.Template

func init() {
	funcMap := template.FuncMap{
		"splitOptions": func(options string) []string {
			if options == "" {
				return nil
			}
			lines := strings.Split(strings.TrimSpace(options), "\n")
			var result []string
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" {
					result = append(result, line)
				}
			}
			return result
		},
		"or": func(args ...bool) bool {
			for _, arg := range args {
				if arg {
					return true
				}
			}
			return false
		},
		"eq": func(a, b any) bool {
			return a == b
		},
	}

	var err error
	tmpl, err = template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		panic(fmt.Sprintf("failed to parse desk templates: %v", err))
	}
}

// Handler holds dependencies for the workspace UI.
type Handler struct {
	Registry *doctype.Registry // fallback for single-site or boot
}

// NewHandler creates a new workspace UI handler.
func NewHandler(registry *doctype.Registry) *Handler {
	return &Handler{Registry: registry}
}

// siteRegistry returns the registry for the current request's site.
func (h *Handler) siteRegistry(c *gin.Context) *doctype.Registry {
	if reg, ok := c.Get("site_registry"); ok {
		if r, ok := reg.(*doctype.Registry); ok && r != nil {
			return r
		}
	}
	return h.Registry
}

// PageData is passed to every rendered template.
type PageData struct {
	Title    string
	SiteName string
	DocTypes []*doctype.DocType
	DocType  *doctype.DocType
	DocName  string
	Data     any
	Error    string
	Message  string
	User     string
	UserRole string
}

// RegisterRoutes registers the admin UI routes on the full engine.
func (h *Handler) RegisterRoutes(router *gin.Engine) {
	h.RegisterRoutesOnGroup(router.Group("/workspace"))
}

// RegisterRoutesOnGroup registers admin UI routes on an existing RouterGroup.
func (h *Handler) RegisterRoutesOnGroup(desk *gin.RouterGroup) {
	desk.GET("/", h.HandleHome)
	desk.GET("/login", h.HandleLoginPage)
	desk.GET("/doctype/:doctype", h.HandleListView)
	desk.GET("/doctype/:doctype/new", h.HandleNewForm)
	desk.GET("/doctype/:doctype/:name", h.HandleEditForm)
	desk.GET("/config", h.HandleConfigView)
}

// HandleHome renders the admin home page.
func (h *Handler) HandleHome(c *gin.Context) {
	data := PageData{
		Title:    "Home",
		SiteName: "Kora",
		DocTypes: h.siteRegistry(c).All(),
		User:     c.GetString("user"),
		UserRole: c.GetString("user_role"),
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(c.Writer, "shell.html", data); err != nil {
		slog.Error("rendering home", "error", err)
	}
}

// HandleLoginPage renders the login form.
func (h *Handler) HandleLoginPage(c *gin.Context) {
	data := PageData{Title: "Login", SiteName: "Kora"}
	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(c.Writer, "login.html", data)
}

// HandleListView renders the document list view for a DocType.
func (h *Handler) HandleListView(c *gin.Context) {
	doctypeName := c.Param("doctype")
	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.String(http.StatusNotFound, "DocType not found")
		return
	}

	data := PageData{
		Title:    dt.Name,
		SiteName: "Kora",
		DocTypes: h.siteRegistry(c).All(),
		DocType:  dt,
		User:     c.GetString("user"),
		UserRole: c.GetString("user_role"),
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(c.Writer, "shell.html", data); err != nil {
		slog.Error("rendering list view", "error", err)
	}
}

// HandleNewForm renders the new document form.
func (h *Handler) HandleNewForm(c *gin.Context) {
	doctypeName := c.Param("doctype")
	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.String(http.StatusNotFound, "DocType not found")
		return
	}

	data := PageData{
		Title:    "New " + dt.Name,
		SiteName: "Kora",
		DocTypes: h.siteRegistry(c).All(),
		DocType:  dt,
		User:     c.GetString("user"),
		UserRole: c.GetString("user_role"),
	}

	// Check for workflow and available transitions.
	if h.siteRegistry(c).Workflows.Has(doctypeName) {
		// New documents start in Draft — no transitions needed yet.
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(c.Writer, "shell.html", data); err != nil {
		slog.Error("rendering new form", "error", err)
	}
}

// HandleEditForm renders the edit document form.
func (h *Handler) HandleEditForm(c *gin.Context) {
	doctypeName := c.Param("doctype")
	name := c.Param("name")
	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.String(http.StatusNotFound, "DocType not found")
		return
	}

	data := PageData{
		Title:    name,
		SiteName: "Kora",
		DocTypes: h.siteRegistry(c).All(),
		DocType:  dt,
		DocName:  name,
		User:     c.GetString("user"),
		UserRole: c.GetString("user_role"),
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(c.Writer, "shell.html", data); err != nil {
		slog.Error("rendering edit form", "error", err)
	}
}

// HandleConfigView renders the config manager page.
func (h *Handler) HandleConfigView(c *gin.Context) {
	data := PageData{
		Title:    "Config Manager",
		SiteName: "Kora",
		DocTypes: h.siteRegistry(c).All(),
		User:     c.GetString("user"),
		UserRole: c.GetString("user_role"),
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(c.Writer, "shell.html", data); err != nil {
		slog.Error("rendering config view", "error", err)
	}
}

// StaticHandler returns a handler that serves the embedded templates for HTMX partials.
func StaticHandler() http.Handler {
	sub, _ := fs.Sub(templateFS, "templates")
	return http.FileServer(http.FS(sub))
}

