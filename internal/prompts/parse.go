package prompts

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// parseTemplate parses raw YAML bytes into a Template. The path is
// stored on the Template for diagnostics and hot-reload decisions.
// Both supported YAML shapes (standard prompts with metadata, and
// memory schemas with memory_type) parse into the same struct; fields
// not present in the YAML stay zero-valued.
func parseTemplate(path string, data []byte) (*Template, error) {
	var t Template
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	t.SourcePath = path
	return &t, nil
}
