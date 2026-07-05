// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionExtractContextProvider_PrefetchSearchQueryMaxChars(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 5000, PrefetchSearchQueryMaxChars)
	assert.Equal(t, 1000, PrefetchSearchTextPartMaxChars)
	assert.Equal(t, 500, PrefetchSearchAssistantTextPartMaxChars)
}

func TestSessionExtractContextProvider_DetectLanguage(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("hello world")}),
	}
	p := NewSessionExtractContextProvider(msgs, "")
	assert.NotEqual(t, "", p.OutputLanguage)
}

func TestSessionExtractContextProvider_DetectLanguage_Empty(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	assert.Equal(t, "en", p.OutputLanguage)
}

func TestSessionExtractContextProvider_Instruction(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	instr := p.Instruction()
	assert.Contains(t, instr, "memory extraction agent")
}

func TestSessionExtractContextProvider_GetTools_EagerPrefetch(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	p.SetEagerPrefetch(true)
	assert.Nil(t, p.GetTools())
}

func TestSessionExtractContextProvider_GetTools_Default(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	assert.Equal(t, []string{"read"}, p.GetTools())
}

func TestSessionExtractContextProvider_Prefetch_NoMessages(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	out, err := p.Prefetch(context.Background())
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestSessionExtractContextProvider_Prefetch_WithMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("I like apples")}),
	}
	p := NewSessionExtractContextProvider(msgs, "")
	out, err := p.Prefetch(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, out)
	assert.Equal(t, "user", out[0]["role"])
	content := out[0]["content"].(string)
	assert.Contains(t, content, "Conversation History")
}

func TestSessionExtractContextProvider_BuildConversationMessage(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "alice", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("hello")}),
		NewTextMessage("2", "assistant", "", "2026-01-01T00:01:00Z",
			[]MessagePart{NewTextOnlyPart("hi there")}),
	}
	p := NewSessionExtractContextProvider(msgs, "")
	msg := p.buildConversationMessage()
	assert.Equal(t, "user", msg["role"])
	content := msg["content"].(string)
	assert.Contains(t, content, "Conversation History")
	assert.Contains(t, content, "Session Time")
}

func TestSessionExtractContextProvider_AssembleConversation(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "alice", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("hello")}),
	}
	p := NewSessionExtractContextProvider(msgs, "")
	conv := p.assembleConversation()
	assert.Contains(t, conv, "hello")
	assert.Contains(t, conv, "user")
}

func TestSessionExtractContextProvider_BuildPrefetchSearchQuery(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("I want to book a flight to Paris")}),
	}
	p := NewSessionExtractContextProvider(msgs, "")
	q := p.buildPrefetchSearchQuery()
	assert.Contains(t, q, "flight")
	assert.Contains(t, q, "Paris")
}

func TestSessionExtractContextProvider_BuildPrefetchSearchQuery_Empty(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	q := p.buildPrefetchSearchQuery()
	assert.Equal(t, "", q)
}

func TestSessionExtractContextProvider_GetExtractContext_Caches(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	ec1 := p.GetExtractContext()
	ec2 := p.GetExtractContext()
	assert.Same(t, ec1, ec2)
}

func TestSessionExtractContextProvider_SetExtractContext(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	ec := NewExtractContext(nil, true)
	p.SetExtractContext(ec)
	assert.Same(t, ec, p.GetExtractContext())
}

func TestSessionExtractContextProvider_ReadFileContents(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	cache := p.ReadFileContents()
	assert.NotNil(t, cache)
	assert.Empty(t, cache)
}

func TestSessionExtractContextProvider_FormatMessageParts(t *testing.T) {
	t.Parallel()
	msg := NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z",
		[]MessagePart{NewTextOnlyPart("line 1"), NewTextOnlyPart("line 2")})
	p := NewSessionExtractContextProvider(nil, "")
	out := p.formatMessageParts(msg)
	assert.Contains(t, out, "line 1")
	assert.Contains(t, out, "line 2")
}

func TestSessionExtractContextProvider_SetIncludeToolParts(t *testing.T) {
	t.Parallel()
	p := NewSessionExtractContextProvider(nil, "")
	p.SetIncludeToolParts(true)
	assert.True(t, p.includeToolParts)
}

func TestExtractURIsFromSearchResult_List(t *testing.T) {
	t.Parallel()
	result := []any{
		map[string]any{"uri": "viking://user/u/memories/profiles/a.md"},
		map[string]any{"uri": "viking://user/u/memories/profiles/b.md"},
	}
	uris := extractURIsFromSearchResult(result, 10)
	assert.Len(t, uris, 2)
	assert.Equal(t, "viking://user/u/memories/profiles/a.md", uris[0])
}

func TestExtractURIsFromSearchResult_Map(t *testing.T) {
	t.Parallel()
	result := map[string]any{
		"memories": []any{
			map[string]any{"uri": "viking://user/u/memories/profiles/c.md"},
		},
	}
	uris := extractURIsFromSearchResult(result, 10)
	assert.Len(t, uris, 1)
	assert.Equal(t, "viking://user/u/memories/profiles/c.md", uris[0])
}

func TestExtractURIsFromSearchResult_Empty(t *testing.T) {
	t.Parallel()
	uris := extractURIsFromSearchResult(nil, 10)
	assert.Empty(t, uris)
}
