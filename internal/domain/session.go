package domain

import "time"

// SessionStatus enumerates the lifecycle states of a Session.
type SessionStatus string

const (
	SessionStatusActive    SessionStatus = "active"
	SessionStatusCommitted SessionStatus = "committed"
	SessionStatusArchived  SessionStatus = "archived"
)

// TurnRole enumerates the role of a single turn in a Session.
type TurnRole string

const (
	TurnRoleUser      TurnRole = "user"
	TurnRoleAssistant TurnRole = "assistant"
	TurnRoleTool      TurnRole = "tool"
)

// TokenUsage accumulates token consumption across a session.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ToolCall records a single tool invocation inside a turn.
type ToolCall struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Args     map[string]any `json:"args,omitempty"`
	Result   string         `json:"result,omitempty"`
	Duration int64          `json:"duration_ms,omitempty"`
}

// Turn is a single message in a Session.
type Turn struct {
	ID        string     `json:"id"`
	Role      TurnRole   `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Tokens    int        `json:"tokens"`
	CreatedAt time.Time  `json:"created_at"`
}

// Session is an Agent conversation with optional extracted memory.
type Session struct {
	ID          string            `json:"id"`
	Account     string            `json:"account"`
	User        string            `json:"user"`
	Peer        string            `json:"peer"`
	Status      SessionStatus     `json:"status"`
	Turns       []Turn            `json:"turns"`
	Summary     string            `json:"summary,omitempty"`
	Memory      []ExtractedMemory `json:"memory,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	CommittedAt *time.Time        `json:"committed_at,omitempty"`
	TokenUsage  TokenUsage        `json:"token_usage"`
}

// ExtractedMemory is a single fact / preference / skill extracted from a session.
type ExtractedMemory struct {
	ID          string         `json:"id"`
	Type        MemoryType     `json:"type"`
	Content     string         `json:"content"`
	SourceTurns []int          `json:"source_turns"`
	Confidence  float64        `json:"confidence"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// MemoryType enumerates the categories of extracted memory.
type MemoryType string

const (
	MemoryTypeFact       MemoryType = "fact"
	MemoryTypePreference MemoryType = "preference"
	MemoryTypeSkill      MemoryType = "skill"
	MemoryTypeEvent      MemoryType = "event"
	MemoryTypeRelation   MemoryType = "relation"
)

// MemoryDiff records changes applied to the memory store after extraction.
type MemoryDiff struct {
	Added    []ExtractedMemory `json:"added"`
	Updated  []ExtractedMemory `json:"updated"`
	Archived []string          `json:"archived"`
}
