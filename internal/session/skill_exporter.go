package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/pkg/vikinguri"
)

// SkillExporter turns a committed Session into a reusable Skill struct
// addressable by a viking:// URI. The export is lossy by design: only
// user / assistant turns are kept as Steps; tool turns are folded into
// the preceding assistant step's description.
type SkillExporter struct{}

// NewSkillExporter returns a SkillExporter.
func NewSkillExporter() *SkillExporter { return &SkillExporter{} }

// Export builds a domain.Skill from sess. sess must be non-nil and have at
// least one user turn; otherwise ErrInsufficientSession is returned. The
// returned Skill's URI is scoped to the session's owner: agent scope when
// the session has no user, user scope otherwise.
func (e *SkillExporter) Export(ctx context.Context, sess *domain.Session) (*domain.Skill, error) {
	if sess == nil {
		return nil, ErrInsufficientSession
	}
	if len(sess.Turns) == 0 {
		return nil, ErrInsufficientSession
	}
	firstUser := ""
	for _, t := range sess.Turns {
		if t.Role == domain.TurnRoleUser && strings.TrimSpace(t.Content) != "" {
			firstUser = strings.TrimSpace(t.Content)
			break
		}
	}
	if firstUser == "" {
		return nil, ErrInsufficientSession
	}
	name := skillName(firstUser)
	scope := scopeFor(sess)
	uri := (&vikinguri.URI{
		Scope: scope,
		Kind:  vikinguri.KindSkills,
		Path:  "/" + name,
	}).String()
	steps := buildSkillSteps(sess.Turns)
	t := now()
	return &domain.Skill{
		URI:         uri,
		Name:        name,
		Level:       0,
		Description: firstUser,
		Trigger:     triggerFrom(firstUser),
		Steps:       steps,
		Files:       nil,
		Metadata: map[string]any{
			"source_session": sess.ID,
			"account":        sess.Account,
			"user":           sess.User,
			"peer":           sess.Peer,
		},
		CreatedAt: t,
		UpdatedAt: t,
	}, nil
}

// ErrInsufficientSession is returned by Export when the session lacks the
// minimum content (a non-empty user turn) required to derive a skill.
var ErrInsufficientSession = errors.New("skill_exporter: session has no usable user turn")

// scopeFor returns the vikinguri.Scope for the session owner. Sessions
// owned by the agent namespace (no user) render as scope "agent"; user
// sessions render as "user_<id>".
func scopeFor(sess *domain.Session) vikinguri.Scope {
	if sess.User == "" {
		return vikinguri.Scope{Type: "agent"}
	}
	return vikinguri.Scope{Type: "user", UserID: sess.User}
}

// skillName derives a kebab-case name from the first user message. The
// name is truncated to 48 runes and stripped of path-unfriendly chars.
func skillName(content string) string {
	content = strings.ToLower(strings.TrimSpace(content))
	// Replace any run of non-alphanumeric with a single hyphen.
	var b strings.Builder
	prevHyphen := false
	n := 0
	for _, r := range content {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			prevHyphen = false
			n++
		} else if r == ' ' || r == '-' || r == '_' {
			if !prevHyphen && n > 0 {
				b.WriteRune('-')
				prevHyphen = true
				n++
			}
		}
		if n >= 48 {
			break
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "skill"
	}
	return name
}

// triggerFrom returns a short trigger phrase derived from the first user
// message. It is the first sentence (or first 60 runes).
func triggerFrom(content string) string {
	content = strings.TrimSpace(content)
	if i := strings.IndexAny(content, ".!?\n"); i > 0 {
		content = content[:i]
	}
	r := []rune(content)
	if len(r) > 60 {
		r = r[:60]
	}
	return string(r)
}

// buildSkillSteps converts the session turns into SkillSteps. Each user or
// assistant turn becomes a step; tool turns are folded into the most
// recent assistant step's description.
func buildSkillSteps(turns []domain.Turn) []domain.SkillStep {
	steps := make([]domain.SkillStep, 0, len(turns))
	order := 0
	for _, t := range turns {
		switch t.Role {
		case domain.TurnRoleUser:
			order++
			steps = append(steps, domain.SkillStep{
				Order:       order,
				Action:      "user",
				Description: t.Content,
			})
		case domain.TurnRoleAssistant:
			order++
			desc := t.Content
			if len(t.ToolCalls) > 0 {
				desc += "\n\nTools: " + formatToolCalls(t.ToolCalls)
			}
			steps = append(steps, domain.SkillStep{
				Order:       order,
				Action:      "assistant",
				Description: desc,
			})
		case domain.TurnRoleTool:
			// Fold into the previous step's description.
			if len(steps) > 0 {
				prev := &steps[len(steps)-1]
				prev.Description += "\n\nTool result: " + t.Content
			}
		}
	}
	return steps
}

// formatToolCalls renders a compact list of tool calls for inclusion in a
// step description.
func formatToolCalls(calls []domain.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, fmt.Sprintf("%s(%s)", c.Name, formatArgs(c.Args)))
	}
	return strings.Join(parts, ", ")
}

// formatArgs renders a tool call's args map as a compact k=v list.
func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ", ")
}
