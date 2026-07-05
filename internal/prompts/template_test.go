package prompts

import (
	"errors"
	"strings"
	"testing"
)

func TestTemplate_Render_MissingRequiredVar(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.missing"},
		Variables: []Variable{
			{Name: "name", Type: "string", Required: true},
		},
		Template: "Hello, {{ name }}!",
	}
	if _, err := tmpl.Render(nil); err == nil {
		t.Fatal("expected error for missing required variable, got nil")
	} else if !strings.Contains(err.Error(), "required variable") {
		t.Fatalf("expected 'required variable' error, got: %v", err)
	}
}

func TestTemplate_Render_ExtraVarsIgnored(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.extra"},
		Variables: []Variable{
			{Name: "name", Type: "string", Required: true},
		},
		Template: "Hello, {{ name }}!",
	}
	vars := map[string]any{
		"name":    "Alice",
		" stray ": "ignored", // keys with whitespace are still accepted
		"another": 42,
	}
	out, err := tmpl.Render(vars)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "Hello, Alice!"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTemplate_Render_NestedVar(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.nested"},
		Variables: []Variable{
			{Name: "user", Type: "string", Required: true},
		},
		Template: "Hello, {{ user.name }} from {{ user.city }}!",
	}
	vars := map[string]any{
		"user": map[string]any{
			"name": "Alice",
			"city": "Wonderland",
		},
	}
	out, err := tmpl.Render(vars)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "Hello, Alice from Wonderland!"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTemplate_Render_DefaultsApplied(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.defaults"},
		Variables: []Variable{
			{Name: "instruction", Type: "string", Default: "Understand image content"},
		},
		Template: "Instruction: {{ instruction }}",
	}
	out, err := tmpl.Render(nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "Instruction: Understand image content"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTemplate_Render_MaxLengthTruncates(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.truncate"},
		Variables: []Variable{
			{Name: "context", Type: "string", MaxLength: 5},
		},
		Template: "Ctx: {{ context }}",
	}
	vars := map[string]any{"context": "abcdefg"}
	out, err := tmpl.Render(vars)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "Ctx: abcde"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTemplate_Render_IfConditional(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.if"},
		Variables: []Variable{
			{Name: "context_type", Type: "string", Required: true},
		},
		Template: `{% if context_type == "resource" %}R{% elif context_type == "memory" %}M{% else %}X{% endif %}`,
	}
	cases := []struct {
		ctx, want string
	}{
		{"resource", "R"},
		{"memory", "M"},
		{"skill", "X"},
	}
	for _, c := range cases {
		out, err := tmpl.Render(map[string]any{"context_type": c.ctx})
		if err != nil {
			t.Fatalf("render %q: %v", c.ctx, err)
		}
		if out != c.want {
			t.Fatalf("ctx=%q: got %q, want %q", c.ctx, out, c.want)
		}
	}
}

func TestTemplate_Render_TruthyIf(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.truthy"},
		Variables: []Variable{
			{Name: "overview", Type: "string"},
		},
		Template: "{% if overview %}Has overview: {{ overview }}{% endif %}Done.",
	}
	out, err := tmpl.Render(map[string]any{"overview": "yes"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "Has overview: yesDone."; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	out, err = tmpl.Render(map[string]any{})
	if err != nil {
		t.Fatalf("render empty: %v", err)
	}
	if want := "Done."; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTemplate_Render_WhitespaceTrimStripped(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.trim"},
		Variables: []Variable{
			{Name: "x", Type: "string", Required: true},
		},
		Template: "{%- if x %}{{- x -}}{%- endif %}",
	}
	out, err := tmpl.Render(map[string]any{"x": "v"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "v"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTemplate_Render_UnsupportedForLoop(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.for"},
		Variables: []Variable{
			{Name: "items", Type: "string"},
		},
		Template: "{% for item in items %}{{ item }}{% endfor %}",
	}
	_, err := tmpl.Render(map[string]any{"items": []string{"a"}})
	if !errors.Is(err, ErrUnsupportedJinja2) {
		t.Fatalf("expected ErrUnsupportedJinja2, got: %v", err)
	}
}

func TestTemplate_Render_UnsupportedFilter(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Metadata: Metadata{ID: "test.filter"},
		Variables: []Variable{
			{Name: "x", Type: "string"},
		},
		Template: "{{ x|default('N/A') }}",
	}
	_, err := tmpl.Render(map[string]any{"x": ""})
	if !errors.Is(err, ErrUnsupportedJinja2) {
		t.Fatalf("expected ErrUnsupportedJinja2, got: %v", err)
	}
}

func TestTranslateJinja2_BareVar(t *testing.T) {
	t.Parallel()
	out, err := translateJinja2("Hello, {{ name }}!")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if want := "Hello, {{.name}}!"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTranslateJinja2_DottedPath(t *testing.T) {
	t.Parallel()
	out, err := translateJinja2("{{ user.name }}")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if want := "{{.user.name}}"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTranslateJinja2_IfElse(t *testing.T) {
	t.Parallel()
	in := `{% if a == "x" %}X{% elif a == "y" %}Y{% else %}Z{% endif %}`
	out, err := translateJinja2(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := `{{if eq .a "x"}}X{{else if eq .a "y"}}Y{{else}}Z{{end}}`
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTranslateJinja2_PreservesProseWithKeyword(t *testing.T) {
	t.Parallel()
	// The word "in" appears in prose but is not inside a Jinja2
	// action; it must not be flagged as unsupported.
	in := "Click the button in the dialog. {{ name }}"
	out, err := translateJinja2(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(out, "{{.name}}") {
		t.Fatalf("missing translated var: %q", out)
	}
	if !strings.Contains(out, "in the dialog") {
		t.Fatalf("prose mangled: %q", out)
	}
}
