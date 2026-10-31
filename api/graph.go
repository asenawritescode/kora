package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

// SystemGraphNode is the canonical, read-only projection used by inspection
// clients. Business schema nodes are always DocTypes; "entity" is not a
// registrable runtime kind.
type SystemGraphNode struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Doctype string `json:"doctype,omitempty"`
}

type SystemGraphEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Relation string `json:"relation"`
	Via      string `json:"via,omitempty"`
}

type SystemGraphResponse struct {
	Nodes []SystemGraphNode `json:"nodes"`
	Edges []SystemGraphEdge `json:"edges"`
}

// BuildSystemGraph projects the active DocType registry into a stable
// inspection graph. It intentionally has no second Entity schema.
func BuildSystemGraph(reg *doctype.Registry) SystemGraphResponse {
	if reg == nil {
		return SystemGraphResponse{Nodes: []SystemGraphNode{}, Edges: []SystemGraphEdge{}}
	}
	doctypeNames := make(map[string]bool)
	nodes := make([]SystemGraphNode, 0)
	for _, dt := range reg.All() {
		if dt == nil || dt.IsChildTable {
			continue
		}
		doctypeNames[dt.Name] = true
		detail := strings.TrimSpace(dt.Module)
		if detail == "" {
			detail = "Core"
		}
		nodes = append(nodes, SystemGraphNode{ID: "doctype:" + dt.Name, Kind: "doctype", Label: dt.Name, Detail: detail + " DocType", Doctype: dt.Name})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	edges := make([]SystemGraphEdge, 0)
	for _, dt := range reg.All() {
		if dt == nil || dt.IsChildTable {
			continue
		}
		for _, field := range dt.Fields {
			if (field.Fieldtype != "Link" && field.Fieldtype != "Dynamic Link") || !doctypeNames[field.Options] {
				continue
			}
			edges = append(edges, SystemGraphEdge{From: "doctype:" + dt.Name, To: "doctype:" + field.Options, Relation: "links to", Via: field.Fieldname})
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Relation < edges[j].Relation
	})
	return SystemGraphResponse{Nodes: nodes, Edges: edges}
}

// HandleSystemGraph returns the canonical DocType-backed inspection graph.
// GET /api/system/graph
func (h *Handler) HandleSystemGraph(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: BuildSystemGraph(h.siteRegistry(c))})
}
