package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

// seedSkill writes a SKILL.md file at rootDir/<name>/SKILL.md on fs.
func seedSkill(t *testing.T, fs *memfs.MemFS, rootDir, name, content string) {
	t.Helper()
	ctx := context.Background()
	path := ComposeSkillPath(rootDir, name)
	if err := fs.Write(ctx, path, bytes.NewReader([]byte(content)), touchFileMode); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func TestSkillInvoker_LoadSkillFromWorkspace(t *testing.T) {
	fs := memfs.New("test")
	root := "/skills"
	seedSkill(t, fs, root, "greet", "---\ndescription: Greets the user\n---\nSay hello.")
	inv := NewSkillInvoker(fs, root)

	content, err := inv.LoadSkill(context.Background(), "greet")
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if !strings.Contains(content, "Say hello.") {
		t.Errorf("content = %q", content)
	}
}

func TestSkillInvoker_LoadSkillNotFound(t *testing.T) {
	fs := memfs.New("test")
	inv := NewSkillInvoker(fs, "/skills")

	_, err := inv.LoadSkill(context.Background(), "missing")
	if err == nil {
		t.Fatalf("LoadSkill should error for missing skill")
	}
	var nf ErrSkillNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err type = %T, want ErrSkillNotFound", err)
	}
	if nf.Name != "missing" {
		t.Errorf("Name = %q", nf.Name)
	}
}

func TestSkillInvoker_LoadSkillsForContext(t *testing.T) {
	fs := memfs.New("test")
	root := "/skills"
	seedSkill(t, fs, root, "a", "---\ndescription: A\n---\nBody A.")
	seedSkill(t, fs, root, "b", "---\ndescription: B\n---\nBody B.")
	inv := NewSkillInvoker(fs, root)

	out, err := inv.LoadSkillsForContext(context.Background(), []string{"a", "b", "missing"})
	if err != nil {
		t.Fatalf("LoadSkillsForContext: %v", err)
	}
	if !strings.Contains(out, "### Skill: a") {
		t.Errorf("missing a section: %q", out)
	}
	if !strings.Contains(out, "### Skill: b") {
		t.Errorf("missing b section: %q", out)
	}
	if !strings.Contains(out, "Body A.") || !strings.Contains(out, "Body B.") {
		t.Errorf("missing body content: %q", out)
	}
	// Frontmatter must be stripped.
	if strings.Contains(out, "description: A") {
		t.Errorf("frontmatter leaked into context: %q", out)
	}
}

func TestSkillInvoker_LoadSkillsForContextEmpty(t *testing.T) {
	fs := memfs.New("test")
	inv := NewSkillInvoker(fs, "/skills")
	out, err := inv.LoadSkillsForContext(context.Background(), []string{"missing"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != "" {
		t.Errorf("out = %q, want empty", out)
	}
}

func TestSkillInvoker_ListSkills(t *testing.T) {
	fs := memfs.New("test")
	root := "/skills"
	seedSkill(t, fs, root, "zeta", "z")
	seedSkill(t, fs, root, "alpha", "a")
	// Seed a directory without SKILL.md — must be skipped.
	_ = fs.Mkdir(context.Background(), root+"/no-skill", 0o755)
	inv := NewSkillInvoker(fs, root)

	skills, err := inv.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("len = %d, want 2", len(skills))
	}
	if skills[0].Name != "alpha" {
		t.Errorf("skills[0].Name = %q", skills[0].Name)
	}
	if skills[1].Name != "zeta" {
		t.Errorf("skills[1].Name = %q", skills[1].Name)
	}
	if skills[0].Source != "workspace" {
		t.Errorf("Source = %q", skills[0].Source)
	}
}

func TestSkillInvoker_ListSkillsEmptyRoot(t *testing.T) {
	// When the root dir does not exist, ListSkills returns an empty
	// list without error (skills dir is optional).
	fs := memfs.New("test")
	inv := NewSkillInvoker(fs, "/skills")
	skills, err := inv.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("len = %d, want 0", len(skills))
	}
}

func TestSkillInvoker_BuildSkillsSummary(t *testing.T) {
	fs := memfs.New("test")
	root := "/skills"
	seedSkill(t, fs, root, "a", "---\ndescription: alpha skill\n---\nBody.")
	inv := NewSkillInvoker(fs, root)

	summary, err := inv.BuildSkillsSummary(context.Background())
	if err != nil {
		t.Fatalf("BuildSkillsSummary: %v", err)
	}
	if !strings.HasPrefix(summary, "<skills>") {
		t.Errorf("summary should start with <skills>: %q", summary)
	}
	if !strings.Contains(summary, "<name>a</name>") {
		t.Errorf("summary missing name: %q", summary)
	}
	if !strings.Contains(summary, "<description>alpha skill</description>") {
		t.Errorf("summary missing description: %q", summary)
	}
}

func TestSkillInvoker_BuildSkillsSummaryEmpty(t *testing.T) {
	fs := memfs.New("test")
	inv := NewSkillInvoker(fs, "/skills")
	summary, err := inv.BuildSkillsSummary(context.Background())
	if err != nil {
		t.Fatalf("BuildSkillsSummary: %v", err)
	}
	if summary != "" {
		t.Errorf("summary = %q, want empty", summary)
	}
}

func TestSkillInvoker_GetSkillMetadata(t *testing.T) {
	fs := memfs.New("test")
	root := "/skills"
	seedSkill(t, fs, root, "a", "---\ndescription: hello world\n---\nBody.")
	inv := NewSkillInvoker(fs, root)

	meta, err := inv.GetSkillMetadata(context.Background(), "a")
	if err != nil {
		t.Fatalf("GetSkillMetadata: %v", err)
	}
	if meta["description"] != "hello world" {
		t.Errorf("description = %q", meta["description"])
	}
}

func TestSkillInvoker_GetSkillDescriptionFallback(t *testing.T) {
	fs := memfs.New("test")
	root := "/skills"
	seedSkill(t, fs, root, "nodesc", "no frontmatter here")
	inv := NewSkillInvoker(fs, root)

	desc, err := inv.GetSkillDescription(context.Background(), "nodesc")
	if err != nil {
		t.Fatalf("GetSkillDescription: %v", err)
	}
	if desc != "nodesc" {
		t.Errorf("desc = %q, want nodesc", desc)
	}
}

func TestSkillInvoker_ExportedSkill(t *testing.T) {
	// Exported skills (from internal/session.SkillExporter) should be
	// loadable alongside SKILL.md workspace skills.
	exported := &domain.Skill{
		URI:         "viking://agent/skills/exported-skill",
		Name:        "exported-skill",
		Description: "An exported skill.",
		Trigger:     "when the user asks for X",
		Steps: []domain.SkillStep{
			{Order: 1, Action: "user", Description: "do thing 1"},
			{Order: 2, Action: "assistant", Description: "respond with X"},
		},
	}
	inv := NewSkillInvoker(nil, "/skills", exported)

	content, err := inv.LoadSkill(context.Background(), "exported-skill")
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if !strings.Contains(content, "An exported skill.") {
		t.Errorf("content missing description: %q", content)
	}
	if !strings.Contains(content, "Trigger: when the user asks for X") {
		t.Errorf("content missing trigger: %q", content)
	}
	if !strings.Contains(content, "1. [user] do thing 1") {
		t.Errorf("content missing step 1: %q", content)
	}
}

func TestSkillInvoker_WorkspacePrecedence(t *testing.T) {
	// When a workspace SKILL.md and an exported skill share a name, the
	// workspace file takes precedence.
	fs := memfs.New("test")
	root := "/skills"
	seedSkill(t, fs, root, "shared", "workspace body")
	exported := &domain.Skill{Name: "shared", Description: "exported body"}
	inv := NewSkillInvoker(fs, root, exported)

	content, err := inv.LoadSkill(context.Background(), "shared")
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if !strings.Contains(content, "workspace body") {
		t.Errorf("expected workspace body, got %q", content)
	}
}

func TestSkillInvoker_RegisterExportedSkill(t *testing.T) {
	inv := NewSkillInvoker(nil, "/skills")
	inv.RegisterExportedSkill(&domain.Skill{Name: "x", Description: "x body"})
	content, err := inv.LoadSkill(context.Background(), "x")
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if !strings.Contains(content, "x body") {
		t.Errorf("content = %q", content)
	}
}

func TestSkillInvoker_NilFSAndNoExported(t *testing.T) {
	inv := NewSkillInvoker(nil, "/skills")
	skills, err := inv.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("len = %d, want 0", len(skills))
	}
}

func TestStripSkillFrontmatter(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"no frontmatter", "hello world", "hello world"},
		{"simple", "---\nkey: value\n---\nbody", "body"},
		{"no closing", "---\nkey: value\nbody", "---\nkey: value\nbody"},
		{"empty body", "---\n---\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StripSkillFrontmatter(c.content)
			if got != c.want {
				t.Errorf("got = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseSkillFrontmatter(t *testing.T) {
	content := "---\nname: test\ndescription: a skill\n---\nbody"
	meta := ParseSkillFrontmatter(content)
	if meta == nil {
		t.Fatalf("meta = nil")
	}
	if meta["name"] != "test" {
		t.Errorf("name = %q", meta["name"])
	}
	if meta["description"] != "a skill" {
		t.Errorf("description = %q", meta["description"])
	}
}

func TestParseSkillFrontmatter_None(t *testing.T) {
	if ParseSkillFrontmatter("no frontmatter") != nil {
		t.Errorf("expected nil for no frontmatter")
	}
}

func TestComposeSkillPath(t *testing.T) {
	got := ComposeSkillPath("/skills", "greet")
	if got != "/skills/greet/SKILL.md" {
		t.Errorf("got = %q", got)
	}
	got = ComposeSkillPath("/skills/", "greet")
	if got != "/skills/greet/SKILL.md" {
		t.Errorf("got = %q", got)
	}
}

func TestEscapeXML(t *testing.T) {
	got := escapeXML("a < b > c & d")
	if got != "a &lt; b &gt; c &amp; d" {
		t.Errorf("got = %q", got)
	}
}

func TestSkillInvoker_RenderExportedSkill(t *testing.T) {
	s := &domain.Skill{
		Name:        "x",
		Description: "desc",
		Trigger:     "trig",
		Steps: []domain.SkillStep{
			{Order: 1, Action: "user", Description: "u"},
			{Order: 2, Action: "assistant", Description: "a"},
		},
	}
	inv := NewSkillInvoker(nil, "/skills")
	out := inv.RenderExportedSkill(s)
	if !strings.Contains(out, "desc") {
		t.Errorf("missing description: %q", out)
	}
	if !strings.Contains(out, "Trigger: trig") {
		t.Errorf("missing trigger: %q", out)
	}
	if !strings.Contains(out, "1. [user] u") {
		t.Errorf("missing step 1: %q", out)
	}
	if !strings.Contains(out, "2. [assistant] a") {
		t.Errorf("missing step 2: %q", out)
	}
}

func TestSkillInvoker_RenderExportedSkillNil(t *testing.T) {
	inv := NewSkillInvoker(nil, "/skills")
	if inv.RenderExportedSkill(nil) != "" {
		t.Errorf("expected empty for nil skill")
	}
}
