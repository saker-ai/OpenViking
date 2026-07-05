package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentifierIsEmpty(t *testing.T) {
	assert.True(t, Identifier{}.IsEmpty())
	assert.True(t, Identifier{User: "u"}.IsEmpty())
	assert.False(t, Identifier{Account: "acct"}.IsEmpty())
}

func TestIdentifierString(t *testing.T) {
	assert.Equal(t, "acct", Identifier{Account: "acct"}.String())
	assert.Equal(t, "acct/u/p", Identifier{Account: "acct", User: "u", ActorPeer: "p"}.String())
}

func TestAppErrorError(t *testing.T) {
	e := NewAppError("CODE_X", 500, "boom")
	assert.Equal(t, "CODE_X: boom", e.Error())

	e2 := &AppError{Code: "CODE_Y"}
	assert.Equal(t, "CODE_Y", e2.Error())
}

func TestAppErrorUnwrap(t *testing.T) {
	inner := errors.New("inner")
	e := Wrap("CODE", 500, inner)
	assert.ErrorIs(t, e, inner)
}

func TestAppErrorWithDetail(t *testing.T) {
	e := NewAppError("CODE", 400, "bad")
	e.WithDetail("field", "value")
	assert.Equal(t, "value", e.Details["field"])
}

func TestSentinelErrors(t *testing.T) {
	assert.Equal(t, 404, ErrNotFound.Status)
	assert.Equal(t, CodeResourceNotFound, ErrNotFound.Code)
	assert.Equal(t, 401, ErrUnauthorized.Status)
	assert.Equal(t, 403, ErrForbidden.Status)
	assert.Equal(t, 409, ErrConflict.Status)
	assert.Equal(t, 422, ErrValidation.Status)
	assert.Equal(t, 429, ErrQuotaExceeded.Status)
	assert.Equal(t, 500, ErrInternal.Status)
	assert.Equal(t, 503, ErrUnavailable.Status)
}

func TestResourceIsDir(t *testing.T) {
	r := Resource{Type: ResourceTypeDir}
	assert.True(t, r.IsDir())

	r2 := Resource{Type: ResourceTypeFile}
	assert.False(t, r2.IsDir())
}

func TestSessionStatusConstants(t *testing.T) {
	assert.Equal(t, SessionStatus("active"), SessionStatusActive)
	assert.Equal(t, SessionStatus("committed"), SessionStatusCommitted)
	assert.Equal(t, SessionStatus("archived"), SessionStatusArchived)
}

func TestTurnRoleConstants(t *testing.T) {
	assert.Equal(t, TurnRole("user"), TurnRoleUser)
	assert.Equal(t, TurnRole("assistant"), TurnRoleAssistant)
	assert.Equal(t, TurnRole("tool"), TurnRoleTool)
}

func TestMemoryTypeConstants(t *testing.T) {
	assert.Equal(t, MemoryType("fact"), MemoryTypeFact)
	assert.Equal(t, MemoryType("preference"), MemoryTypePreference)
	assert.Equal(t, MemoryType("skill"), MemoryTypeSkill)
	assert.Equal(t, MemoryType("event"), MemoryTypeEvent)
	assert.Equal(t, MemoryType("relation"), MemoryTypeRelation)
}

func TestResourceTypeConstants(t *testing.T) {
	cases := []ResourceType{
		ResourceTypeFile, ResourceTypeDir, ResourceTypeURL,
		ResourceTypeMemory, ResourceTypeSkill, ResourceTypeSession, ResourceTypeRelation,
	}
	for _, rt := range cases {
		assert.NotEmpty(t, string(rt))
	}
}
