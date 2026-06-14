package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/yourorg/kora/doctype"
)

// HandleOpenAPI returns the OpenAPI 3.x spec for the current site.
func (h *Handler) HandleOpenAPI(c *gin.Context) {
	reg := h.siteRegistry(c)
	siteName := c.GetString("site_name")
	if siteName == "" {
		siteName = "default"
	}
	spec := GenerateOpenAPISpec(reg, siteName)
	c.JSON(http.StatusOK, spec)
}

// HandleSwaggerUI serves the Swagger UI HTML page.
func (h *Handler) HandleSwaggerUI(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, swaggerUIHTML)
}

// Shortcuts for building spec elements.
func resp(desc string) *openapi3.ResponseRef {
	return &openapi3.ResponseRef{Value: &openapi3.Response{Description: &desc}}
}

func ref(name string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{
		Ref:   "#/components/schemas/" + name,
		Value: openapi3.NewObjectSchema(), // Value must be non-nil for JSON serialization
	}
}

// GenerateOpenAPISpec builds the full OpenAPI 3.x document from a registry.
func GenerateOpenAPISpec(reg *doctype.Registry, siteName string) *openapi3.T {
	spec := &openapi3.T{
		OpenAPI: "3.0.3",
		Info: &openapi3.Info{
			Title:       fmt.Sprintf("Kora — %s API", siteName),
			Description: "Auto-generated API documentation. Endpoints reflect the currently loaded doctypes.",
			Version:     "1.0.0",
		},
		Servers:    openapi3.Servers{{URL: "/api", Description: siteName + " API"}},
		Paths:      &openapi3.Paths{},
		Components: &openapi3.Components{Schemas: make(map[string]*openapi3.SchemaRef)},
	}

	addStaticPaths(spec)
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		addDoctypePaths(spec, dt)
	}
	for _, dt := range reg.All() {
		spec.Components.Schemas[dt.Name] = doctypeToSchemaRef(dt, reg)
		spec.Components.Schemas[dt.Name+"List"] = &openapi3.SchemaRef{
			Value: openapi3.NewArraySchema().WithItems(ref(dt.Name).Value),
		}
	}
	return spec
}

func addStaticPaths(spec *openapi3.T) {
	spec.Paths.Set("/api/ping", &openapi3.PathItem{
		Get: &openapi3.Operation{
			OperationID: "ping",
			Summary:     "Health check",
			Responses:   openapi3.NewResponses(openapi3.WithStatus(200, resp("pong"))),
		},
	})
	spec.Paths.Set("/api/auth/login", &openapi3.PathItem{
		Post: &openapi3.Operation{
			OperationID: "login",
			Summary:     "Authenticate and get session cookie",
			RequestBody: &openapi3.RequestBodyRef{
				Value: openapi3.NewRequestBody().
					WithRequired(true).
					WithJSONSchema(openapi3.NewObjectSchema().
						WithProperty("email", openapi3.NewStringSchema()).
						WithProperty("password", openapi3.NewStringSchema())),
			},
			Responses: openapi3.NewResponses(
				openapi3.WithStatus(200, resp("Session created")),
				openapi3.WithStatus(401, resp("Invalid credentials")),
			),
		},
	})
	spec.Paths.Set("/api/auth/logout", &openapi3.PathItem{
		Post: &openapi3.Operation{
			OperationID: "logout", Summary: "Destroy current session",
			Responses: openapi3.NewResponses(openapi3.WithStatus(200, resp("Logged out"))),
		},
	})
	spec.Paths.Set("/api/auth/me", &openapi3.PathItem{
		Get: &openapi3.Operation{
			OperationID: "me", Summary: "Get current user info",
			Responses: openapi3.NewResponses(openapi3.WithStatus(200, resp("User object"))),
		},
	})
}

func addDoctypePaths(spec *openapi3.T, dt *doctype.DocType) {
	name := dt.Name
	listPath := "/api/resource/" + name
	detailPath := listPath + "/{name}"
	opID := strings.ToLower(name)

	spec.Paths.Set(listPath, &openapi3.PathItem{
		Get: &openapi3.Operation{
			OperationID: opID + "_list", Summary: "List " + name,
			Responses: openapi3.NewResponses(openapi3.WithStatus(200, resp("Array of "+name))),
		},
		Post: &openapi3.Operation{
			OperationID: opID + "_create", Summary: "Create " + name,
			RequestBody: &openapi3.RequestBodyRef{
				Value: openapi3.NewRequestBody().WithRequired(true).WithJSONSchemaRef(ref(name)),
			},
			Responses: openapi3.NewResponses(
				openapi3.WithStatus(201, resp(name+" created")),
				openapi3.WithStatus(400, resp("Validation error")),
			),
		},
	})
	spec.Paths.Set(detailPath, &openapi3.PathItem{
		Get: &openapi3.Operation{
			OperationID: opID + "_get", Summary: "Get " + name,
			Responses: openapi3.NewResponses(openapi3.WithStatus(200, resp(name+" document"))),
		},
		Put: &openapi3.Operation{
			OperationID: opID + "_update", Summary: "Update " + name,
			RequestBody: &openapi3.RequestBodyRef{
				Value: openapi3.NewRequestBody().WithRequired(true).WithJSONSchemaRef(ref(name)),
			},
			Responses: openapi3.NewResponses(openapi3.WithStatus(200, resp(name+" updated"))),
		},
		Delete: &openapi3.Operation{
			OperationID: opID + "_delete", Summary: "Delete " + name,
			Responses: openapi3.NewResponses(openapi3.WithStatus(200, resp(name+" deleted"))),
		},
	})
	if dt.IsSubmittable {
		spec.Paths.Set(detailPath+"/workflow_action", &openapi3.PathItem{
			Post: &openapi3.Operation{
				OperationID: opID + "_workflow", Summary: "Execute workflow action on " + name,
				RequestBody: &openapi3.RequestBodyRef{
					Value: openapi3.NewRequestBody().WithRequired(true).
						WithJSONSchema(openapi3.NewObjectSchema().WithProperty("action", openapi3.NewStringSchema())),
				},
				Responses: openapi3.NewResponses(openapi3.WithStatus(200, resp("Transition applied"))),
			},
		})
	}
}

func doctypeToSchemaRef(dt *doctype.DocType, reg *doctype.Registry) *openapi3.SchemaRef {
	s := openapi3.NewObjectSchema()
	s.WithProperty("name", openapi3.NewStringSchema())
	s.WithProperty("doc_status", openapi3.NewIntegerSchema())
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" && f.Options != "" {
			if childDT := reg.Get(f.Options); childDT != nil {
				s.WithPropertyRef(f.Fieldname, &openapi3.SchemaRef{
					Value: openapi3.NewArraySchema().WithItems(doctypeToSchemaRef(childDT, reg).Value),
				})
			}
			continue
		}
		s.WithProperty(f.Fieldname, fieldToSchema(&f))
	}
	return &openapi3.SchemaRef{Value: s}
}

func fieldToSchema(f *doctype.Field) *openapi3.Schema {
	switch f.Fieldtype {
	case "Int":
		return openapi3.NewIntegerSchema()
	case "Float", "Currency", "Percent":
		return openapi3.NewFloat64Schema()
	case "Check":
		return openapi3.NewBoolSchema()
	case "Select":
		s := openapi3.NewStringSchema()
		for _, v := range strings.Split(f.Options, "\n") {
			if v != "" {
				s.Enum = append(s.Enum, v)
			}
		}
		return s
	default:
		return openapi3.NewStringSchema()
	}
}

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Kora API Docs</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    SwaggerUIBundle({
      url: "/api/openapi.json",
      dom_id: "#swagger-ui",
      deepLinking: true
    })
  </script>
</body>
</html>`
