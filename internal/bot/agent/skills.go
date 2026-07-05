// Package agent: skill invocation.
//
// SkillInvoker loads SKILL.md files from a ragfs.FileSystem, parses
// their YAML-like frontmatter, strips it for inline inclusion in the
// agent prompt, and exposes a summary the agent can use to pick which
// skill to read in full. This is the Go counterpart of
// bot/vikingbot/agent/skills.py.
//
// Reuse of internal/session/skill_exporter.go: the SkillExporter turns
// a committed Session into a domain.Skill (URI + steps). SkillInvoker
// can render such a domain.Skill as markdown via RenderExportedSkill,
// so the agent loop can include either form (SKILL.md file or exported
// session-derived skill) using the same surface.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// SkillFileSystem is the subset of ragfs.FileSystem SkillInvoker uses.
// Declared as a seam so tests can inject an in-memory backend without
// pulling in the full plugin stack.
type SkillFileSystem interface {
	Read(ctx context.Context, path string, w io.Writer) error
	ReadDir(ctx context.Context, path string) ([]*ragfs.TreeEntry, error)
}

// SkillLocation describes where a skill lives. The Location is the
// ragfs path to the SKILL.md file (workspace skills) or the viking://
// URI of an exported domain.Skill.
type SkillLocation struct {
	Name     string // directory name (workspace) or skill name (exported)
	Path     string // ragfs path or viking URI
	Source   string // "workspace" or "exported"
}

// SkillInvoker loads SKILL.md files from a ragfs FileSystem. The zero
// value is NOT usable; use NewSkillInvoker.
type SkillInvoker struct {
	fs       SkillFileSystem
	rootDir  string // ragfs path under which skills/<name>/SKILL.md live
	exported map[string]*domain.Skill
}

// NewSkillInvoker returns an invoker that loads SKILL.md files from fs
// rooted at rootDir (typically "/skills"). exportedSkills may be nil;
// any non-nil entries are registered under their Name field and can be
// rendered via RenderExportedSkill.
func NewSkillInvoker(fs SkillFileSystem, rootDir string, exportedSkills ...*domain.Skill) *SkillInvoker {
	inv := &SkillInvoker{
		fs:       fs,
		rootDir:  strings.TrimRight(rootDir, "/"),
		exported: make(map[string]*domain.Skill),
	}
	for _, s := range exportedSkills {
		if s != nil && s.Name != "" {
			inv.exported[s.Name] = s
		}
	}
	return inv
}

// RegisterExportedSkill adds (or replaces) a session-derived skill
// produced by internal/session.SkillExporter. The skill is addressable
// by its Name field alongside SKILL.md workspace skills.
func (inv *SkillInvoker) RegisterExportedSkill(s *domain.Skill) {
	if s == nil || s.Name == "" {
		return
	}
	inv.exported[s.Name] = s
}

// ListSkills returns the skills available to the agent. Workspace
// SKILL.md files take precedence over exported skills with the same
// name (mirrors the Python "workspace first" priority).
func (inv *SkillInvoker) ListSkills(ctx context.Context) ([]SkillLocation, error) {
	out := []SkillLocation{}
	if inv.fs != nil {
		entries, err := inv.fs.ReadDir(ctx, inv.rootDir)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("skills: list %s: %w", inv.rootDir, err)
		}
		for _, e := range entries {
			if e == nil || e.Info == nil || !e.Info.IsDir {
				continue
			}
			skillMD := inv.rootDir + "/" + e.Info.Name + "/SKILL.md"
			if _, err := inv.readFile(ctx, skillMD); err != nil {
				continue
			}
			out = append(out, SkillLocation{
				Name:   e.Info.Name,
				Path:   skillMD,
				Source: "workspace",
			})
		}
	}
	for name, s := range inv.exported {
		already := false
		for _, l := range out {
			if l.Name == name {
				already = true
				break
			}
		}
		if already {
			continue
		}
		out = append(out, SkillLocation{
			Name:   name,
			Path:   s.URI,
			Source: "exported",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LoadSkill returns the raw SKILL.md content for name. Workspace skills
// take precedence over exported skills.
func (inv *SkillInvoker) LoadSkill(ctx context.Context, name string) (string, error) {
	if inv.fs != nil {
		workspacePath := inv.rootDir + "/" + name + "/SKILL.md"
		if content, err := inv.readFile(ctx, workspacePath); err == nil {
			return content, nil
		}
	}
	if s, ok := inv.exported[name]; ok {
		return inv.RenderExportedSkill(s), nil
	}
	return "", ErrSkillNotFound{Name: name}
}

// LoadSkillsForContext returns a formatted markdown blob for the named
// skills, with frontmatter stripped. Skills that cannot be loaded are
// skipped silently. Returns "" when no skill loads successfully.
func (inv *SkillInvoker) LoadSkillsForContext(ctx context.Context, names []string) (string, error) {
	parts := []string{}
	for _, name := range names {
		content, err := inv.LoadSkill(ctx, name)
		if err != nil {
			continue
		}
		content = StripSkillFrontmatter(content)
		if strings.TrimSpace(content) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("### Skill: %s\n\n%s", name, content))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// BuildSkillsSummary returns an XML-formatted summary of all available
// skills. Empty when no skills are available.
func (inv *SkillInvoker) BuildSkillsSummary(ctx context.Context) (string, error) {
	skills, err := inv.ListSkills(ctx)
	if err != nil {
		return "", err
	}
	if len(skills) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("<skills>\n")
	for _, s := range skills {
		name := escapeXML(s.Name)
		path := escapeXML(s.Path)
		desc, _ := inv.GetSkillDescription(ctx, s.Name)
		desc = escapeXML(desc)
		b.WriteString(fmt.Sprintf("  <skill available=\"true\">\n"))
		b.WriteString(fmt.Sprintf("    <name>%s</name>\n", name))
		b.WriteString(fmt.Sprintf("    <description>%s</description>\n", desc))
		b.WriteString(fmt.Sprintf("    <location>%s</location>\n", path))
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</skills>")
	return b.String(), nil
}

// GetSkillMetadata parses the YAML-like frontmatter of the named skill
// into a flat string-string map. Returns nil when the skill has no
// frontmatter. The parser is intentionally minimal (key: value lines)
// to match the Python counterpart; complex YAML is out of scope.
func (inv *SkillInvoker) GetSkillMetadata(ctx context.Context, name string) (map[string]string, error) {
	content, err := inv.LoadSkill(ctx, name)
	if err != nil {
		return nil, err
	}
	return ParseSkillFrontmatter(content), nil
}

// GetSkillDescription returns the description frontmatter field, or the
// skill name when the field is absent / the skill has no frontmatter.
func (inv *SkillInvoker) GetSkillDescription(ctx context.Context, name string) (string, error) {
	meta, err := inv.GetSkillMetadata(ctx, name)
	if err != nil {
		return name, nil
	}
	if d, ok := meta["description"]; ok && d != "" {
		return d, nil
	}
	return name, nil
}

// RenderExportedSkill turns a domain.Skill (produced by
// internal/session.SkillExporter) into a markdown body the agent can
// include in its prompt. Steps are rendered as an ordered list.
func (inv *SkillInvoker) RenderExportedSkill(s *domain.Skill) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	if s.Description != "" {
		b.WriteString(s.Description)
		b.WriteString("\n\n")
	}
	if s.Trigger != "" {
		b.WriteString("Trigger: ")
		b.WriteString(s.Trigger)
		b.WriteString("\n\n")
	}
	if len(s.Steps) == 0 {
		return strings.TrimSpace(b.String())
	}
	b.WriteString("Steps:\n")
	for _, st := range s.Steps {
		b.WriteString(fmt.Sprintf("%d. [%s] %s\n", st.Order, st.Action, st.Description))
	}
	return strings.TrimSpace(b.String())
}

// readFile is a thin wrapper around SkillFileSystem.Read that returns
// the file content as a string. Errors are passed through unchanged.
func (inv *SkillInvoker) readFile(ctx context.Context, path string) (string, error) {
	if inv.fs == nil {
		return "", ErrSkillNotFound{Path: path}
	}
	var buf bytes.Buffer
	if err := inv.fs.Read(ctx, path, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ErrSkillNotFound is returned by LoadSkill when no SKILL.md exists for
// the requested name (and no exported skill matches).
type ErrSkillNotFound struct {
	Name string
	Path string
}

func (e ErrSkillNotFound) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("skills: not found at %s", e.Path)
	}
	return fmt.Sprintf("skills: %q not found", e.Name)
}

// StripSkillFrontmatter removes a leading YAML frontmatter block
// (--- ... ---) from content and returns the trimmed body. Mirrors the
// Python _strip_frontmatter helper.
func StripSkillFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	// Find the closing "---" on its own line.
	rest := content[3:]
	if !strings.HasPrefix(rest, "\n") {
		// "---..." without a newline is not a frontmatter fence.
		return content
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return content
	}
	after := rest[idx+len("\n---"):]
	// Skip a single trailing newline after the fence.
	after = strings.TrimPrefix(after, "\n")
	return strings.TrimSpace(after)
}

// ParseSkillFrontmatter parses a YAML-like frontmatter block into a
// flat map. The parser handles "key: value" lines only; nested maps
// and arrays are out of scope. Returns nil when content has no
// frontmatter.
func ParseSkillFrontmatter(content string) map[string]string {
	if !strings.HasPrefix(content, "---") {
		return nil
	}
	rest := content[3:]
	if !strings.HasPrefix(rest, "\n") {
		return nil
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil
	}
	body := rest[1:idx]
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		val = strings.Trim(val, "\"'")
		if key != "" {
			out[key] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// escapeXML replaces the four characters that matter for XML/HTML
// content. Mirrors the Python escape_xml helper.
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ComposeSkillPath returns the ragfs path of a workspace skill's
// SKILL.md file. Exposed so callers can reference the file in tool
// descriptions.
func ComposeSkillPath(rootDir, name string) string {
	return strings.TrimRight(rootDir, "/") + "/" + name + "/SKILL.md"
}

// touchFileMode is the default file mode for skills written through the
// invoker (used by tests that seed a memfs via Write). 0o644 matches
// the ragfs memfs default.
const touchFileMode os.FileMode = 0o644
