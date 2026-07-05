package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/version"
)

// openapiInfo is the OpenAPI 3.0 metadata block served at /openapi.json.
// It mirrors the Python FastAPI get_openapi() shape so web-studio's
// `pnpm gen-server-client` script can regenerate the SDK from the Go
// server's live spec (script/gen-server-client/gen-server-client.sh).
type openapiInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

// openapiSpec is the minimal OpenAPI 3.0 document served at /openapi.json.
// Paths are enumerated from gin's router tree so the spec stays in sync
// with the actual route set without a manual maintenance burden.
// Schemas are kept permissive (any-typed) because the Go server does not
// carry Pydantic-style model metadata; the Studio SDK regenerates with
// `result: unknown` shapes which is enough for path correctness.
type openapiSpec struct {
	OpenAPI    string                            `json:"openapi"`
	Info       openapiInfo                       `json:"info"`
	Paths      map[string]map[string]any         `json:"paths"`
	Components map[string]map[string]any         `json:"components,omitempty"`
}

// registerOpenAPI wires GET /openapi.json onto the engine. The handler
// walks gin's route tree at request time so the served spec always
// matches the registered route set — no spec file to keep in sync.
func registerOpenAPI(r *gin.Engine) {
	r.GET("/openapi.json", func(c *gin.Context) {
		spec := buildOpenAPISpec(r)
		c.JSON(http.StatusOK, spec)
	})
}

// buildOpenAPISpec constructs the OpenAPI 3.0 document from gin's route
// tree. Each route becomes a path entry with its method; the response
// schema is the standard {status, result} envelope so the regenerated
// SDK's `getOvResult` helper continues to unwrap result correctly.
func buildOpenAPISpec(r *gin.Engine) openapiSpec {
	paths := make(map[string]map[string]any)
	for _, ri := range r.Routes() {
		path := convertPathParams(ri.Path)
		if _, ok := paths[path]; !ok {
			paths[path] = make(map[string]any)
		}
		// Skip OPTIONS — gin auto-responds to OPTIONS for CORS; we don't
		// want to surface every route as an OPTIONS endpoint in the spec.
		if ri.Method == http.MethodOptions {
			continue
		}
		paths[path][methodLower(ri.Method)] = map[string]any{
			"summary": pathSummary(path, ri.Method),
			"responses": map[string]any{
				"200": map[string]any{
					"description": "OK",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"status": map[string]any{"type": "string"},
									"result": map[string]any{},
								},
							},
						},
					},
				},
			},
		}
	}
	return openapiSpec{
		OpenAPI: "3.0.0",
		Info: openapiInfo{
			Title:       "OpenViking Server",
			Description: "Go rewrite of the OpenViking HTTP API surface.",
			Version:     version.Version,
		},
		Paths: paths,
		Components: map[string]map[string]any{
			"securitySchemes": map[string]any{
				"ApiKeyAuth": map[string]any{
					"type": "apiKey",
					"in":   "header",
					"name": "X-OpenViking-Account",
				},
			},
		},
	}
}

// convertPathParams rewrites gin-style :param path segments to OpenAPI
// {param} form so the regenerated SDK's path builder can substitute them.
func convertPathParams(p string) string {
	out := make([]byte, 0, len(p))
	i := 0
	for i < len(p) {
		if p[i] == ':' {
			// Read the parameter name (until / or end).
			j := i + 1
			for j < len(p) && p[j] != '/' {
				j++
			}
			name := p[i+1 : j]
			out = append(out, '{')
			out = append(out, name...)
			out = append(out, '}')
			i = j
			continue
		}
		// Wildcard /*uri -> {uri} (collapse the * into the brace form).
		if p[i] == '*' && i+1 < len(p) {
			j := i + 1
			for j < len(p) && p[j] != '/' {
				j++
			}
			name := p[i+1 : j]
			out = append(out, '{')
			out = append(out, name...)
			out = append(out, '}')
			i = j
			continue
		}
		out = append(out, p[i])
		i++
	}
	return string(out)
}

// methodLower lowercases an HTTP method for OpenAPI's paths.<method> key.
func methodLower(m string) string {
	out := make([]byte, len(m))
	for i := 0; i < len(m); i++ {
		c := m[i]
		if c >= 'A' && c <= 'Z' {
			c = c + 32
		}
		out[i] = c
	}
	return string(out)
}

// pathSummary returns a human-readable summary for a route based on its
// path + method. The summary shows up in the regenerated SDK's operation
// names so Studio's API calls have a readable label.
func pathSummary(path, method string) string {
	// Strip query strings and use the last meaningful segment.
	last := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			last = path[i+1:]
			break
		}
	}
	if last == "" {
		last = "root"
	}
	return method + " " + last
}
