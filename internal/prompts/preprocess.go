package prompts

import (
	"fmt"
	"regexp"
	"strings"
)

// translateJinja2 converts the Jinja2-subset used by OpenViking prompt
// templates into Go text/template syntax. The supported subset matches
// what the 43 templates actually use outside the memory schemas (cases,
// events, etc., which use {% set %}, {% for %}, filters, and function
// calls — those features are reported as ErrUnsupportedJinja2).
//
// Supported:
//   - {{ var }}              -> {{ .var }}
//   - {{ var.attr }}         -> {{ .var.attr }}
//   - {{ var == "literal" }} -> {{ eq .var "literal" }}
//   - {{ var != "literal" }} -> {{ ne .var "literal" }}
//   - {% if cond %}          -> {{if cond}}
//   - {% elif cond %}        -> {{else if cond}}
//   - {% else %}             -> {{else}}
//   - {% endif %}            -> {{end}}
//   - {%- ... %} / {% ... -%} whitespace trim markers silently stripped
//
// Unsupported (returns ErrUnsupportedJinja2):
//   - {% for x in y %} / {% endfor %}
//   - {% set x = y %}
//   - {{ x or y }}
//   - {{ x|filter }}
//   - {{ "needle" in haystack }}
//   - function calls (uri_basename(...), extract_context.x(...), etc.)
func translateJinja2(src string) (string, error) {
	// Strip whitespace-trim markers FIRST so subsequent scanning and
	// translation sees clean {% ... %} / {{ ... }} actions. Go text/
	// template has no whitespace control equivalent; the trim
	// semantics are silently dropped.
	src = strings.ReplaceAll(src, "{%-", "{%")
	src = strings.ReplaceAll(src, "-%}", "%}")
	src = strings.ReplaceAll(src, "{{-", "{{")
	src = strings.ReplaceAll(src, "-}}", "}}")

	// Walk every Jinja2 action ({% ... %} or {{ ... }}) and reject
	// unsupported constructs. This avoids false matches in
	// surrounding prose (e.g. the English word "in" inside a
	// description block).
	if err := scanForUnsupported(src); err != nil {
		return "", err
	}

	out := actionRegexp.ReplaceAllStringFunc(src, func(m string) string {
		open, close := "{{", "}}"
		body := m[2 : len(m)-2]
		body = strings.TrimSpace(body)
		if strings.HasPrefix(m, "{%") {
			translated := translateControl(body)
			return open + translated + close
		}
		// {{ ... }} expression. Unsupported expressions were already
		// rejected by scanForUnsupported; leave anything translateExpr
		// can't classify as-is so the parse step surfaces a precise
		// error.
		translated := translateExpr(body)
		return open + translated + close
	})

	return out, nil
}

// actionRegexp matches {% ... %} and {{ ... }} blocks. The character
// class [^}%] keeps adjacent blocks from merging and stops at the
// first closing delimiter — acceptable for the supported subset
// (literals with embedded braces are not used in the 43 templates).
var actionRegexp = regexp.MustCompile(`\{[{%][^}%]*?[}%]\}`)

// scanForUnsupported walks every Jinja2 action in src and rejects
// expressions or control keywords outside the supported subset.
func scanForUnsupported(src string) error {
	for _, m := range actionRegexp.FindAllString(src, -1) {
		body := strings.TrimSpace(m[2 : len(m)-2])
		if strings.HasPrefix(m, "{%") {
			if err := checkControlSupported(body); err != nil {
				return err
			}
		} else {
			if err := checkExprSupported(body); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkControlSupported rejects {% for %}, {% set %}, {% filter %},
// {% macro %}, {% block %}, {% extends %}, {% include %}, {% with %},
// {% without %}. Allows if/elif/else/endif/endfor-only-when-it-pairs.
func checkControlSupported(body string) error {
	first := firstWord(body)
	switch first {
	case "if", "elif", "else", "endif":
		if first == "if" || first == "elif" {
			return checkExprSupported(strings.TrimSpace(body[len(first):]))
		}
		return nil
	case "endfor":
		// endfor only appears paired with for, which is rejected
		// below. If we ever reach endfor without a for, treat as
		// unsupported so the error is precise.
		return fmt.Errorf("%w: unsupported control %q", ErrUnsupportedJinja2, body)
	case "for", "set", "filter", "macro", "block", "extends", "include", "with", "without":
		return fmt.Errorf("%w: unsupported control %q", ErrUnsupportedJinja2, body)
	default:
		return fmt.Errorf("%w: unsupported control %q", ErrUnsupportedJinja2, body)
	}
}

// checkExprSupported rejects expressions containing `or`, `and`, `in`,
// `|filter`, function calls, or arithmetic operators outside the
// supported equality form.
func checkExprSupported(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}
	// Quoted string literal.
	if isQuoted(expr) {
		return nil
	}
	// Reject pipe filters (e.g. {{ x|default('') }}).
	if strings.Contains(expr, "|") {
		return fmt.Errorf("%w: filter expression %q", ErrUnsupportedJinja2, expr)
	}
	// Reject `or` / `and` / `in` as standalone words.
	if wordBoundaryRegexp.MatchString(expr) {
		return fmt.Errorf("%w: expression %q", ErrUnsupportedJinja2, expr)
	}
	// Reject function-call form `name(args)` or `obj.method(args)`.
	if callRegexp.MatchString(expr) {
		return fmt.Errorf("%w: function call %q", ErrUnsupportedJinja2, expr)
	}
	// Allow equality / inequality and bare identifiers / dotted paths.
	return nil
}

// translateControl maps a Jinja2 control keyword to the equivalent Go
// text/template action (without delimiters).
func translateControl(inner string) string {
	switch {
	case strings.HasPrefix(inner, "if "):
		return "if " + translateCond(inner[3:])
	case inner == "endif":
		return "end"
	case strings.HasPrefix(inner, "elif "):
		return "else if " + translateCond(inner[5:])
	case inner == "else":
		return "else"
	default:
		// Unsupported constructs were already rejected.
		return inner
	}
}

// translateCond converts a Jinja2 condition to Go text/template syntax.
// Supports: bare variable truthiness, `var == "literal"`, `var != "literal"`.
func translateCond(expr string) string {
	expr = strings.TrimSpace(expr)
	if eq := strings.Index(expr, "=="); eq >= 0 {
		left := strings.TrimSpace(expr[:eq])
		right := strings.TrimSpace(expr[eq+2:])
		return "eq " + translateExpr(left) + " " + right
	}
	if neq := strings.Index(expr, "!="); neq >= 0 {
		left := strings.TrimSpace(expr[:neq])
		right := strings.TrimSpace(expr[neq+2:])
		return "ne " + translateExpr(left) + " " + right
	}
	return translateExpr(expr)
}

// translateExpr converts a Jinja2 expression (inside {{ }} or a
// condition) to Go text/template syntax. The only transformation is
// prepending "." to bare identifiers so they refer to the data root.
// Quoted strings, numbers, and already-dotted paths are preserved.
func translateExpr(expr string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return ""
	}
	if isQuoted(expr) {
		return expr
	}
	if isNumeric(expr) {
		return expr
	}
	// Bare identifier or dotted path: prepend "." to the head only.
	parts := strings.SplitN(expr, ".", 2)
	head := parts[0]
	if !isBareIdentifier(head) {
		return expr
	}
	out := "." + head
	if len(parts) == 2 {
		out += "." + parts[1]
	}
	return out
}

// firstWord returns the leading whitespace-delimited token of s.
func firstWord(s string) string {
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			return s[:i]
		}
	}
	return s
}

// isQuoted reports whether s is wrapped in matching single or double
// quotes (no internal unescaped quotes — good enough for the supported
// subset where literals are short).
func isQuoted(s string) bool {
	if len(s) < 2 {
		return false
	}
	first, last := s[0], s[len(s)-1]
	return (first == '"' && last == '"') || (first == '\'' && last == '\'')
}

// isBareIdentifier reports whether s is a simple identifier (letter
// or underscore, followed by letters, digits, or underscores).
func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' {
			continue
		}
		if i == 0 && !isLetter(r) {
			return false
		}
		if !(isLetter(r) || isDigit(r)) {
			return false
		}
	}
	return true
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

// isNumeric reports whether s is an integer or float literal.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	for i, r := range s {
		if r == '.' {
			if dot || i == 0 || i == len(s)-1 {
				return false
			}
			dot = true
			continue
		}
		if !isDigit(r) {
			return false
		}
	}
	return true
}

// wordBoundaryRegexp matches the standalone words `or`, `and`, `in`,
// `not in`, `is`. Used to reject boolean/subscript operators that
// Go text/template cannot express without custom funcs.
var wordBoundaryRegexp = regexp.MustCompile(`\b(?:or|and|in|is)\b`)

// callRegexp matches a function-call form `name(...)` or `obj.method(...)`.
var callRegexp = regexp.MustCompile(`\b\w+\s*\([^)]*\)`)
