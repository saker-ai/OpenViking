package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestSkillExporterExportUserScope(t *testing.T) {
	ex := NewSkillExporter()
	sess := &domain.Session{
		ID:      newSessionID(),
		Account: "acct",
		User:    "alice",
		Peer:    "p",
		Status:  domain.SessionStatusCommitted,
		Turns: []domain.Turn{
			{ID: newTurnID(), Role: domain.TurnRoleUser, Content: "Build a REST API in Go"},
			{ID: newTurnID(), Role: domain.TurnRoleAssistant, Content: "Use gin and gorm", ToolCalls: []domain.ToolCall{
				{ID: "tc1", Name: "write_file", Args: map[string]any{"path": "main.go"}},
			}},
			{ID: newTurnID(), Role: domain.TurnRoleTool, Content: "wrote 200 bytes"},
		},
	}
	sk, err := ex.Export(context.Background(), sess)
	require.NoError(t, err)
	require.NotNil(t, sk)
	assert.Equal(t, "viking://user_alice/skills/build-a-rest-api-in-go", sk.URI)
	assert.Equal(t, "build-a-rest-api-in-go", sk.Name)
	assert.Equal(t, "Build a REST API in Go", sk.Description)
	assert.Equal(t, "Build a REST API in Go", sk.Trigger)
	assert.Equal(t, 0, sk.Level)
	assert.Equal(t, "acct", sk.Metadata["account"])
	assert.Equal(t, "alice", sk.Metadata["user"])
	assert.Equal(t, sess.ID, sk.Metadata["source_session"])
	// 2 user/assistant steps; tool turn folded into the assistant step.
	require.Len(t, sk.Steps, 2)
	assert.Equal(t, "user", sk.Steps[0].Action)
	assert.Equal(t, 1, sk.Steps[0].Order)
	assert.Equal(t, "assistant", sk.Steps[1].Action)
	assert.Equal(t, 2, sk.Steps[1].Order)
	assert.Contains(t, sk.Steps[1].Description, "Tools: write_file(path=main.go)")
	assert.Contains(t, sk.Steps[1].Description, "Tool result: wrote 200 bytes")
}

func TestSkillExporterExportAgentScope(t *testing.T) {
	ex := NewSkillExporter()
	sess := &domain.Session{
		ID:      newSessionID(),
		Account: "acct",
		Status:  domain.SessionStatusCommitted,
		Turns: []domain.Turn{
			{ID: newTurnID(), Role: domain.TurnRoleUser, Content: "Hello"},
			{ID: newTurnID(), Role: domain.TurnRoleAssistant, Content: "Hi there"},
		},
	}
	sk, err := ex.Export(context.Background(), sess)
	require.NoError(t, err)
	assert.Equal(t, "viking://agent/skills/hello", sk.URI)
}

func TestSkillExporterExportNoUserTurn(t *testing.T) {
	ex := NewSkillExporter()
	sess := &domain.Session{
		ID:      newSessionID(),
		Account: "acct",
		Turns: []domain.Turn{
			{ID: newTurnID(), Role: domain.TurnRoleAssistant, Content: "Hi"},
		},
	}
	_, err := ex.Export(context.Background(), sess)
	assert.ErrorIs(t, err, ErrInsufficientSession)
}

func TestSkillExporterExportEmptySession(t *testing.T) {
	ex := NewSkillExporter()
	_, err := ex.Export(context.Background(), &domain.Session{ID: "x"})
	assert.ErrorIs(t, err, ErrInsufficientSession)
}

func TestSkillExporterExportNilSession(t *testing.T) {
	ex := NewSkillExporter()
	_, err := ex.Export(context.Background(), nil)
	assert.ErrorIs(t, err, ErrInsufficientSession)
}

func TestSkillExporterSkillNameTruncationAndKebab(t *testing.T) {
	long := skillName("Build a REST API in Go with gin and gorm and redis and postgres")
	assert.LessOrEqual(t, len([]rune(long)), 48)
	assert.True(t, startsWithPrefix(long, "build-a-rest"))
	assert.Equal(t, "hello-world", skillName("  Hello, World!  "))
	assert.Equal(t, "skill", skillName("!!!@@@"))
	assert.Equal(t, "abc-123", skillName("abc 123"))
}

func startsWithPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestSkillExporterTriggerTruncation(t *testing.T) {
	long := "This is a very long first sentence that should be truncated at sixty runes to keep the trigger field short and readable"
	got := triggerFrom(long)
	assert.LessOrEqual(t, len([]rune(got)), 60)
}

func TestSkillExporterTriggerFirstSentence(t *testing.T) {
	assert.Equal(t, "Hello world", triggerFrom("Hello world. Second sentence."))
}
