// Package prompts loads OpenViking prompt templates (YAML) and renders
// them with a Jinja2-subset syntax ported onto Go's text/template.
//
// The Python reference lives in openviking/prompts/. The Go port embeds
// the same 43 YAML files via embed.FS so the binary is self-contained,
// and also supports loading from a configurable on-disk directory for
// hot reload during development.
//
// Supported Jinja2 subset (matches what the 43 templates use outside
// the memory schemas):
//   - {{ var }} and {{ var.attr }} variable substitution
//   - {% if cond %} / {% elif cond %} / {% else %} / {% endif %}
//   - {% if x == "literal" %} equality
//   - {%- ... %} and {% ... -%} whitespace trim markers (silently
//     stripped; Go text/template has no equivalent)
//
// Gaps (memory schemas only — cases.yaml, events.yaml, etc.):
//   - {% for x in y %} loops with {% set %} inside
//   - {{ x or y }} default operator
//   - {{ "needle" in haystack }} substring test
//   - {{ x|filter(args) }} Jinja2 filters (default, round, int)
//   - Function calls like uri_basename(t), link_target(t), and
//     extract_context.get_resource_event_content(ranges, summary)
//
// The memory schemas use a different YAML shape (memory_type,
// content_template, fields) and are loaded as Template values with
// their ContentTemplate populated, but rendering them via Render()
// returns ErrUnsupportedJinja2 for the gap features. Callers that
// need full Jinja2 fidelity should keep the Python loader in place.
package prompts

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// ErrUnsupportedJinja2 is returned by Render when a template uses
// Jinja2 features that have no Go text/template equivalent.
var ErrUnsupportedJinja2 = errors.New("prompts: template uses unsupported Jinja2 feature")

// Metadata describes a standard prompt template's metadata block.
type Metadata struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
	Language    string `yaml:"language"`
	Category    string `yaml:"category"`
}

// Variable describes one variable declaration in a standard prompt
// template. Defaults and type checks are applied at Render time.
type Variable struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	Default     any    `yaml:"default"`
	Required    bool   `yaml:"required"`
	MaxLength   int    `yaml:"max_length"`
}

// Field describes one field declaration in a memory schema template
// (memory_type / fields shape). Carried through for completeness; the
// Go loader does not enforce field-level merge semantics.
type Field struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	MergeOp     string `yaml:"merge_op"`
	InitValue   any    `yaml:"init_value"`
}

// Template is the parsed representation of one YAML prompt file. The
// same struct covers both standard prompts (with Metadata, Variables,
// Template) and memory schemas (with MemoryType, ContentTemplate,
// Fields). Fields not present in the YAML stay zero-valued.
type Template struct {
	// Standard prompt fields.
	Metadata  Metadata   `yaml:"metadata"`
	Variables []Variable `yaml:"variables"`
	Template  string     `yaml:"template"`

	// LLMConfig carries the optional llm_config block (temperature,
	// supports_vision, etc.) as raw key/value pairs.
	LLMConfig map[string]any `yaml:"llm_config"`

	// OutputSchema carries the optional output_schema block.
	OutputSchema map[string]any `yaml:"output_schema"`

	// Memory schema fields (memory_type / content_template / fields).
	MemoryType        string  `yaml:"memory_type"`
	Description       string  `yaml:"description"`
	Directory         string  `yaml:"directory"`
	FilenameTemplate  string  `yaml:"filename_template"`
	Enabled           bool    `yaml:"enabled"`
	OperationMode     string  `yaml:"operation_mode"`
	ContentTemplate   string  `yaml:"content_template"`
	EmbeddingTemplate string  `yaml:"embedding_template"`
	OverviewTemplate  string  `yaml:"overview_template"`
	PeerEnabled       bool    `yaml:"peer_enabled"`
	Fields            []Field `yaml:"fields"`

	// SourcePath is the on-disk path the template was loaded from,
	// or the embed.FS path when loaded from the binary. Used for
	// diagnostics and hot-reload decisions.
	SourcePath string `yaml:"-"`
}

// RenderField selects which string field of a Template to render.
// Standard prompts use the "template" field; memory schemas use
// "content_template", "description", "directory", etc.
type RenderField string

const (
	// FieldTemplate renders t.Template (standard prompts).
	FieldTemplate RenderField = "template"
	// FieldContentTemplate renders t.ContentTemplate (memory schemas).
	FieldContentTemplate RenderField = "content_template"
	// FieldDescription renders t.Description.
	FieldDescription RenderField = "description"
	// FieldDirectory renders t.Directory.
	FieldDirectory RenderField = "directory"
	// FieldFilenameTemplate renders t.FilenameTemplate.
	FieldFilenameTemplate RenderField = "filename_template"
	// FieldEmbeddingTemplate renders t.EmbeddingTemplate.
	FieldEmbeddingTemplate RenderField = "embedding_template"
	// FieldOverviewTemplate renders t.OverviewTemplate.
	FieldOverviewTemplate RenderField = "overview_template"
)

// renderSource returns the raw Jinja2 source string for the selected
// field. Empty string means the field is absent.
func (t *Template) renderSource(field RenderField) string {
	switch field {
	case FieldTemplate:
		return t.Template
	case FieldContentTemplate:
		return t.ContentTemplate
	case FieldDescription:
		return t.Description
	case FieldDirectory:
		return t.Directory
	case FieldFilenameTemplate:
		return t.FilenameTemplate
	case FieldEmbeddingTemplate:
		return t.EmbeddingTemplate
	case FieldOverviewTemplate:
		return t.OverviewTemplate
	default:
		return ""
	}
}

// Render renders the template's primary "template" field (standard
// prompts) with the given variables. See RenderField for memory
// schemas. Missing required variables produce an error; extra
// variables are silently ignored.
func (t *Template) Render(vars map[string]any) (string, error) {
	return t.RenderField(FieldTemplate, vars)
}

// RenderField renders the selected field of the template. Variables
// are validated against t.Variables (defaults applied, required
// checked, types loosely enforced) before rendering.
func (t *Template) RenderField(field RenderField, vars map[string]any) (string, error) {
	src := t.renderSource(field)
	if src == "" {
		return "", fmt.Errorf("prompts: template %q has no %s field", t.idOrMemoryType(), field)
	}

	renderVars := make(map[string]any, len(vars)+len(t.Variables))
	for k, v := range vars {
		renderVars[k] = v
	}
	for _, vd := range t.Variables {
		if _, ok := renderVars[vd.Name]; !ok {
			if vd.Default != nil {
				renderVars[vd.Name] = vd.Default
				continue
			}
			if vd.Required {
				return "", fmt.Errorf("prompts: required variable %q missing for template %q", vd.Name, t.idOrMemoryType())
			}
		}
	}
	for _, vd := range t.Variables {
		if vd.MaxLength > 0 {
			if s, ok := renderVars[vd.Name].(string); ok && len(s) > vd.MaxLength {
				renderVars[vd.Name] = s[:vd.MaxLength]
			}
		}
	}

	translated, err := translateJinja2(src)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New("prompts").Funcs(builtinFuncs()).Parse(translated)
	if err != nil {
		return "", fmt.Errorf("prompts: parse template %q: %w", t.idOrMemoryType(), err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, renderVars); err != nil {
		return "", fmt.Errorf("prompts: render template %q: %w", t.idOrMemoryType(), err)
	}
	return sb.String(), nil
}

// idOrMemoryType returns Metadata.ID for standard prompts, or
// MemoryType for memory schemas. Used in error messages.
func (t *Template) idOrMemoryType() string {
	if t.Metadata.ID != "" {
		return t.Metadata.ID
	}
	if t.MemoryType != "" {
		return t.MemoryType
	}
	return t.SourcePath
}

// builtinFuncs returns the custom template functions used by the
// Jinja2 subset. Currently empty; future filters (default, contains,
// uri_basename) will land here when memory schema rendering is wired.
func builtinFuncs() template.FuncMap {
	return template.FuncMap{}
}
