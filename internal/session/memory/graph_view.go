// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MemoryTypeColors maps memory type names to display colors for the
// graph view. It mirrors openviking.session.memory.graph_view.TYPE_COLORS.
var MemoryTypeColors = map[string]string{
	"profile":      "#e74c3c",
	"preferences":  "#3498db",
	"entities":     "#2ecc71",
	"events":       "#f39c12",
	"skills":       "#9b59b6",
	"identity":     "#1abc9c",
	"tools":        "#e67e22",
	"experiences":  "#fd79a8",
	"trajectories": "#6c5ce7",
}

// ColorForLinkType returns a stable HSL color for the given link type.
// It mirrors openviking.session.memory.graph_view._color_for_link_type.
func ColorForLinkType(linkType string) string {
	normalized := strings.TrimSpace(linkType)
	if normalized == "" {
		normalized = "related_to"
	}
	digest := sha256.Sum256([]byte(normalized))
	hexStr := hex.EncodeToString(digest[:4])
	// Parse first 8 hex chars as unsigned int.
	var v uint32
	for i := 0; i < 8 && i < len(hexStr); i++ {
		c := hexStr[i]
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		}
		v = v*16 + d
	}
	hue := int(v % 360)
	return fmt.Sprintf("hsl(%d, 72%%, 58%%)", hue)
}

// GraphFS is the filesystem subset used by MemoryGraph. It extends the
// read-only VikingFS interface with WriteFile so the rendered HTML can
// be persisted back to the memory space. Callers that already implement
// VikingFS only need to add WriteFile to satisfy this interface.
type GraphFS interface {
	VikingFS
	WriteFile(ctx context.Context, uri, content string, requestCtx any) error
}

// MemoryGraph generates a self-contained D3.js force-directed HTML graph
// from all links stored in MEMORY_FIELDS across one or more memory spaces.
// It mirrors openviking.session.memory.graph_view.MemoryGraph.
type MemoryGraph struct {
	vikingFS GraphFS
}

// NewMemoryGraph returns a MemoryGraph that reads from fs. When fs is
// nil, callers must call SetVikingFS before BuildGraph/GenGraph.
func NewMemoryGraph(fs GraphFS) *MemoryGraph {
	return &MemoryGraph{vikingFS: fs}
}

// SetVikingFS swaps the underlying filesystem. Used by tests and by
// callers that lazily acquire the FS.
func (g *MemoryGraph) SetVikingFS(fs GraphFS) { g.vikingFS = fs }

// BuildContentPreview truncates content to limit characters, appending
// an ellipsis. It mirrors MemoryGraph._build_content_preview.
func BuildContentPreview(content string, limit int) string {
	if limit <= 0 {
		limit = 600
	}
	text := strings.TrimSpace(content)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// IsContentTruncated reports whether content exceeds the preview limit.
// It mirrors MemoryGraph._is_content_truncated.
func IsContentTruncated(content string, limit int) bool {
	if limit <= 0 {
		limit = 600
	}
	return len(strings.TrimSpace(content)) > limit
}

// InferMemoryType derives the memory type from a URI when the parsed
// memory_type is empty. It mirrors MemoryGraph._infer_memory_type.
func InferMemoryType(uri, parsedMemoryType string) string {
	if parsedMemoryType != "" {
		return parsedMemoryType
	}
	parts := [16]string{}
	idx := 0
	start := 0
	for i := 0; i < len(uri); i++ {
		if uri[i] == '/' {
			if i > start {
				parts[idx] = uri[start:i]
				idx++
				if idx >= len(parts) {
					break
				}
			}
			start = i + 1
		}
	}
	if start < len(uri) && idx < len(parts) {
		parts[idx] = uri[start:]
		idx++
	}
	allParts := parts[:idx]
	if len(allParts) < 2 {
		return ""
	}
	last := allParts[len(allParts)-1]
	if !strings.HasSuffix(last, ".md") {
		return ""
	}
	parent := allParts[len(allParts)-2]
	if parent == "memories" {
		return strings.TrimSuffix(last, ".md")
	}
	return parent
}

// CollectGraphData reads all .md files under each space URI and extracts
// nodes (memory files) and edges (links). It mirrors
// MemoryGraph._collect_graph_data.
func (g *MemoryGraph) CollectGraphData(ctx context.Context, spaceURIs []string, requestCtx any) ([]map[string]any, []map[string]any, error) {
	if g.vikingFS == nil {
		return nil, nil, fmt.Errorf("VikingFS not available")
	}
	nodes := make(map[string]map[string]any, 16)
	edges := make([]map[string]any, 0, 16)
	for _, spaceURI := range spaceURIs {
		entries, err := g.vikingFS.Ls(ctx, spaceURI, "agent", 1000000, false, 1000000, requestCtx)
		if err != nil {
			continue
		}
		mdURIs := make([]string, 0, len(entries))
		for _, e := range entries {
			if isDir, _ := e["isDir"].(bool); isDir {
				continue
			}
			relPath, _ := e["rel_path"].(string)
			if relPath == "" {
				relPath, _ = e["name"].(string)
			}
			if !strings.HasSuffix(relPath, ".md") {
				continue
			}
			if strings.HasSuffix(relPath, "/.overview.md") || strings.HasSuffix(relPath, "/.abstract.md") {
				continue
			}
			if relPath == ".overview.md" || relPath == ".abstract.md" {
				continue
			}
			uri, _ := e["uri"].(string)
			if uri == "" {
				uri = spaceURI + "/" + relPath
			}
			mdURIs = append(mdURIs, uri)
		}
		for _, uri := range mdURIs {
			content, err := g.vikingFS.ReadFile(ctx, uri, requestCtx)
			if err != nil || content == "" {
				continue
			}
			mf := MemoryFileRead(content, uri)
			inferredType := InferMemoryType(uri, mf.MemoryType)
			category, _ := mf.ExtraFields["category"].(string)
			name, _ := mf.ExtraFields["name"].(string)
			label := name
			if label == "" {
				if idx := strings.LastIndex(uri, "/"); idx >= 0 {
					label = strings.TrimSuffix(uri[idx+1:], ".md")
				} else {
					label = uri
				}
			}
			renderedContent := RenderLinks(mf.Content, uri, mf.Links)
			nodes[uri] = map[string]any{
				"id":                uri,
				"uri":               uri,
				"label":             label,
				"memory_type":       inferredType,
				"category":          category,
				"content_preview":   BuildContentPreview(renderedContent, 600),
				"content_full":      renderedContent,
				"content_truncated": IsContentTruncated(renderedContent, 600),
			}
			for _, linkData := range mf.Links {
				toURI, _ := linkData["to_uri"].(string)
				if toURI == "" {
					continue
				}
				fromURI, _ := linkData["from_uri"].(string)
				if fromURI == "" {
					fromURI = uri
				}
				linkType, _ := linkData["link_type"].(string)
				if linkType == "" {
					linkType = "related_to"
				}
				weight := 1.0
				if w, ok := linkData["weight"].(float64); ok {
					weight = w
				}
				description, _ := linkData["description"].(string)
				edges = append(edges, map[string]any{
					"source":      fromURI,
					"target":      toURI,
					"link_type":   linkType,
					"weight":      weight,
					"description": description,
				})
			}
		}
	}
	// Drop edges that reference nodes not in the set; dedupe by (source,target,link_type).
	seen := make(map[string]struct{}, len(edges))
	uniqueEdges := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		src, _ := e["source"].(string)
		tgt, _ := e["target"].(string)
		if _, ok := nodes[src]; !ok {
			continue
		}
		if _, ok := nodes[tgt]; !ok {
			continue
		}
		linkType, _ := e["link_type"].(string)
		key := src + "\x00" + tgt + "\x00" + linkType
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		uniqueEdges = append(uniqueEdges, e)
	}
	// Sort nodes by URI for deterministic output.
	nodeList := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		nodeList = append(nodeList, n)
	}
	sort.Slice(nodeList, func(i, j int) bool {
		a, _ := nodeList[i]["id"].(string)
		b, _ := nodeList[j]["id"].(string)
		return a < b
	})
	return nodeList, uniqueEdges, nil
}

// GenGraph scans a single memory space and writes the graph HTML to
// <space>/.graph.html. It mirrors MemoryGraph.gen_graph.
func (g *MemoryGraph) GenGraph(ctx context.Context, spaceURI string, requestCtx any) (string, error) {
	graphPath := strings.TrimRight(spaceURI, "/") + "/.graph.html"
	return g.BuildGraph(ctx, []string{spaceURI}, graphPath, requestCtx)
}

// BuildGraph scans multiple memory roots, extracts links, builds the
// graph HTML, and writes it to outputURI. It mirrors MemoryGraph.build_graph.
func (g *MemoryGraph) BuildGraph(ctx context.Context, spaceURIs []string, outputURI string, requestCtx any) (string, error) {
	if len(spaceURIs) == 0 {
		return "", fmt.Errorf("space_uris must not be empty")
	}
	if outputURI == "" {
		return "", fmt.Errorf("output_uri must not be empty")
	}
	if g.vikingFS == nil {
		return "", fmt.Errorf("VikingFS not available")
	}
	nodes, edges, err := g.CollectGraphData(ctx, spaceURIs, requestCtx)
	if err != nil {
		return "", err
	}
	html := RenderGraphHTML(nodes, edges)
	if err := g.vikingFS.WriteFile(ctx, outputURI, html, requestCtx); err != nil {
		return "", fmt.Errorf("write graph %s: %w", outputURI, err)
	}
	return outputURI, nil
}

// RenderGraphHTML renders the self-contained HTML page for the graph
// view. It mirrors openviking.session.memory.graph_view._render_graph_html.
//
// The HTML loads vis-network from a CDN and embeds the nodes/edges as
// JSON. The page is intentionally self-contained: no external CSS or
// JS beyond the vis-network CDN bundle.
func RenderGraphHTML(nodes, edges []map[string]any) string {
	visNodes := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		mt, _ := node["memory_type"].(string)
		color := MemoryTypeColors[mt]
		if color == "" {
			color = "#64748b"
		}
		visNodes = append(visNodes, map[string]any{
			"id":    node["id"],
			"label": node["label"],
			"shape": "box",
			"color": map[string]any{
				"background": "#0f172a",
				"border":     color,
				"highlight":  map[string]any{"background": "#f8fafc", "border": color},
				"hover":      map[string]any{"background": "#0f172a", "border": color},
			},
			"font":               map[string]any{"color": "#f8fafc", "size": 12},
			"margin":             10,
			"widthConstraint":    map[string]any{"minimum": 120, "maximum": 180},
			"memory_type":        mt,
			"category":           node["category"],
			"uri":                node["uri"],
			"content_preview":    node["content_preview"],
			"content_full":       node["content_full"],
			"content_truncated":  node["content_truncated"],
		})
	}
	visEdges := make([]map[string]any, 0, len(edges))
	for idx, edge := range edges {
		linkType, _ := edge["link_type"].(string)
		if linkType == "" {
			linkType = "related_to"
		}
		color := ColorForLinkType(linkType)
		weight := 1.0
		if w, ok := edge["weight"].(float64); ok {
			weight = w
		}
		width := 2.0
		if weight*4 > 2 {
			width = weight * 4
		}
		visEdges = append(visEdges, map[string]any{
			"id":          fmt.Sprintf("edge-%d", idx),
			"from":        edge["source"],
			"to":          edge["target"],
			"label":       linkType,
			"color":       map[string]any{"color": color, "highlight": color, "hover": color},
			"dashes":      false,
			"width":       width,
			"arrows":      "to",
			"smooth":      map[string]any{"type": "dynamic"},
			"link_type":   linkType,
			"weight":      weight,
			"description": edge["description"],
		})
	}
	nodesJSON := scriptSafeJSON(visNodes)
	edgesJSON := scriptSafeJSON(visEdges)
	typeColorsJSON := scriptSafeJSON(MemoryTypeColors)
	return graphHTMLTemplate(nodesJSON, edgesJSON, typeColorsJSON)
}

// scriptSafeJSON serializes v to JSON with </ escaped to <\/ so the
// result can be safely embedded in a <script> block. It mirrors
// _script_safe_json.
func scriptSafeJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return strings.ReplaceAll(string(data), "</", `<\/`)
}

// graphHTMLTemplate returns the self-contained HTML page. The template
// is intentionally minimal — the Python original ships a richer D3 view
// with markdown rendering and legend filters; the Go counterpart keeps
// the core vis-network scaffold so callers can render a working graph.
func graphHTMLTemplate(nodesJSON, edgesJSON, typeColorsJSON string) string {
	const tpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Memory Graph</title>
<style>
  * { box-sizing: border-box; }
  body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, sans-serif; background: #0f172a; color: #e2e8f0; }
  #graph { width: 100%%; height: 100vh; background: radial-gradient(circle at top, #1e293b 0%%, #0f172a 60%%); }
  #legend { position: fixed; left: 16px; top: 16px; z-index: 10; background: rgba(15, 23, 42, 0.92); border: 1px solid #334155; border-radius: 12px; padding: 12px 14px; max-width: 300px; }
  #legend h4 { margin: 0 0 8px; font-size: 13px; }
  .legend-item { display: flex; align-items: center; gap: 8px; margin: 4px 0; font-size: 12px; color: #cbd5e1; }
  .legend-chip { width: 12px; height: 12px; border-radius: 4px; flex: 0 0 12px; }
</style>
<script src="https://unpkg.com/vis-network/standalone/umd/vis-network.min.js"></script>
</head>
<body>
<div id="legend"><h4>Memory Types</h4><div id="legend-items"></div></div>
<div id="graph"></div>
<script>
const originalNodes = %s;
const originalEdges = %s;
const typeColors = %s;
const legendItems = document.getElementById('legend-items');
if (!window.vis || !window.vis.Network) {
  document.getElementById('graph').innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100%%;padding:24px;text-align:center;">Graph library failed to load.</div>';
} else {
  const nodes = new vis.DataSet(originalNodes);
  const edges = new vis.DataSet(originalEdges);
  const network = new vis.Network(document.getElementById('graph'), { nodes, edges }, {
    autoResize: true,
    interaction: { hover: true, tooltipDelay: 100 },
    physics: { stabilization: { iterations: 250 }, barnesHut: { gravitationalConstant: -7000, centralGravity: 0.18, springLength: 120, springConstant: 0.02, damping: 0.24 } },
  });
  Object.entries(typeColors).forEach(([k, v]) => {
    const item = document.createElement('div');
    item.className = 'legend-item';
    item.innerHTML = '<span class="legend-chip" style="background:' + v + '"></span><span>' + k + '</span>';
    legendItems.appendChild(item);
  });
  network.once('stabilized', () => network.fit({ animation: false, padding: 80 }));
}
</script>
</body>
</html>`
	return fmt.Sprintf(tpl, nodesJSON, edgesJSON, typeColorsJSON)
}
