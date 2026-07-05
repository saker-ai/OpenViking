package domain

import "time"

// Skill is a reusable Agent capability stored under viking://agent/skills or
// viking://user_xxx/skills.
type Skill struct {
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Level       int            `json:"level"`
	Description string         `json:"description"`
	Trigger     string         `json:"trigger"`
	Steps       []SkillStep    `json:"steps"`
	Files       []string       `json:"files"`
	Embedding   []float32      `json:"-"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// SkillStep is a single instruction inside a skill.
type SkillStep struct {
	Order       int            `json:"order"`
	Action      string         `json:"action"`
	Description string         `json:"description"`
	Args        map[string]any `json:"args,omitempty"`
}
