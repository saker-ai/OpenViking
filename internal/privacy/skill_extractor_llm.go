package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file ports the LLM-based extraction path from Python
// openviking/privacy/skill_extractor.py. The Go version originally
// shipped with only the regex approximation (ExtractSkillPrivacyValues
// in skill_extractor.go) which assigns sequential field names like
// "field_1", "field_2". This file adds the LLM path that assigns
// semantic field names like "user_email", "api_key" — matching the
// Python behavior.
//
// Design notes:
//
//  1. The VLM client and prompt renderer are injected via interfaces
//     to avoid a circular dependency (privacy -> vlm -> domain, and
//     privacy is imported by bot/agent which is upstream of vlm).
//     Callers wire the real adapters at startup; tests inject stubs.
//
//  2. The LLM is expected to return JSON shaped like
//     {"values": {"field_name": "raw value or empty string"}}. Empty
//     string values are preserved — Python treats them as "field is
//     required but no concrete value present", which is exactly the
//     semantic we want for skill authors who haven't filled in a real
//     secret yet.
//
//  3. On any LLM error (network, auth, non-JSON, malformed shape),
//     callers should fall back to the regex path via
//     ExtractSkillPrivacyValuesWithFallback. The fallback never
//     fails — it always returns a usable result.

// VLMClient is the minimal VLM interface the LLM extractor needs. It
// matches *vlm.OpenAIClient.Chat by shape, so callers can pass the real
// client directly without an adapter.
type VLMClient interface {
	Chat(ctx context.Context, req VLMChatRequest) (*VLMChatResponse, error)
}

// VLMChatRequest is the request shape passed to VLMClient.Chat. Only
// the fields the LLM extractor populates are present; providers ignore
// the rest.
type VLMChatRequest struct {
	Model       string            `json:"model,omitempty"`
	Messages    []VLMMessage      `json:"messages"`
	Temperature float64           `json:"temperature,omitempty"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	// ResponseFormatJSON requests JSON output. Providers that don't
	// support structured output ignore this; the extractor still
	// parses the response text via parseJSONFromResponse.
	ResponseFormatJSON bool `json:"response_format_json,omitempty"`
}

// VLMMessage is a single chat message.
type VLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// VLMChatResponse is the response shape returned by VLMClient.Chat.
type VLMChatResponse struct {
	Content      string `json:"content"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// PromptRenderer renders the skill.privacy_extraction prompt template
// with the given variables. Callers typically pass prompts.Store.Render
// adapted to this signature.
type PromptRenderer interface {
	Render(skillName, skillDescription, content string) (string, error)
}

// PromptRendererFunc lets a plain function satisfy PromptRenderer.
type PromptRendererFunc func(skillName, skillDescription, content string) (string, error)

// Render implements PromptRenderer.
func (f PromptRendererFunc) Render(skillName, skillDescription, content string) (string, error) {
	return f(skillName, skillDescription, content)
}

// ExtractSkillPrivacyValuesWithLLM calls a VLM to identify the
// user-maintained private config items in the given skill content and
// returns the placeholderized result with semantic field names.
//
// Mirrors the Python extract_skill_privacy_values. The VLM is asked
// (via the skill.privacy_extraction prompt) to return JSON shaped like
// {"values": {"field_name": "raw value or empty string"}}. The values
// map is then run through PlaceholderizeSkillContentWithBlocks so the
// placeholderize/restore round-trip logic stays shared with the regex
// path.
//
// Returns:
//   - The extraction result on success.
//   - An error if the VLM call fails, the response is not valid JSON,
//     or the JSON shape is wrong. Callers should fall back to the
//     regex path (ExtractSkillPrivacyValues, which never fails) when
//     this returns an error.
//
// The VLM is allowed to return empty string values for fields that are
// clearly required but missing a concrete value (Python behavior). Such
// entries are kept in Values so the placeholderize step still produces
// a placeholder for the field — RestoreSkillContent will surface them
// as "field=<missing>" if no real value is provided at restore time.
//
// ctx is used for VLM call cancellation/timeout. The renderer is
// invoked synchronously; if it fails the function returns an error
// without calling the VLM.
func ExtractSkillPrivacyValuesWithLLM(
	ctx context.Context,
	skillName, skillDescription, content string,
	client VLMClient,
	renderer PromptRenderer,
) (*SkillPrivacyExtractionResult, error) {
	if client == nil {
		return nil, errors.New("privacy: VLM client is nil")
	}
	if renderer == nil {
		return nil, errors.New("privacy: prompt renderer is nil")
	}
	prompt, err := renderer.Render(skillName, skillDescription, content)
	if err != nil {
		return nil, fmt.Errorf("privacy: render prompt: %w", err)
	}
	resp, err := client.Chat(ctx, VLMChatRequest{
		Messages: []VLMMessage{
			{Role: "user", Content: prompt},
		},
		Temperature:        0.0,
		ResponseFormatJSON: true,
	})
	if err != nil {
		return nil, fmt.Errorf("privacy: vlm chat: %w", err)
	}
	values, err := parseLLMExtractionResponse(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("privacy: parse vlm response: %w", err)
	}
	placeholderResult := PlaceholderizeSkillContentWithBlocks(content, skillName, values)
	return &SkillPrivacyExtractionResult{
		Values:                   placeholderResult.ReplacedValues,
		OriginalContent:          content,
		SanitizedContent:         placeholderResult.SanitizedContent,
		OriginalContentBlocks:    placeholderResult.OriginalContentBlocks,
		ReplacementContentBlocks: placeholderResult.ReplacementContentBlocks,
	}, nil
}

// parseLLMExtractionResponse parses the raw text returned by the VLM
// into the values map. Mirrors the Python parse_json_from_response +
// shape validation. Tolerates:
//   - JSON with surrounding prose (extracts the first {...} block)
//   - Markdown ```json fences
//   - null values (treated as empty string, Python: `"" if value is None else str(value)`)
//   - non-string values (stringified via fmt.Sprint-style coercion)
//   - {"values": null} or missing "values" key (treated as empty map)
//
// Always returns a usable map (possibly empty); only returns an error
// when the response contains no JSON object at all or the JSON is
// unparseable. This matches the Python behavior where any malformed
// shape degrades to an empty values dict rather than raising.
func parseLLMExtractionResponse(raw string) (map[string]string, error) {
	jsonStr, ok := extractJSONObject(raw)
	if !ok {
		return nil, errors.New("no JSON object in response")
	}
	// Decode into map[string]any so non-string values (numbers, bools,
	// null) can be stringified like Python's str(value).
	var parsed struct {
		Values map[string]any `json:"values"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	out := make(map[string]string)
	for k, v := range parsed.Values {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = stringifyLLMValue(v)
	}
	return out, nil
}

// stringifyLLMValue mirrors the Python `"" if value is None else str(value)`
// normalization. Numbers use their Go default formatting; bools become
// "true"/"false"; nil becomes ""; other types fall back to fmt.Sprint.
func stringifyLLMValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers always come back as float64. Trim trailing
		// ".0" for integer-valued numbers so 8080 stays "8080".
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// extractJSONObject locates the first balanced {...} substring in s,
// tolerating string literals containing braces. Returns the JSON
// substring and a bool indicating whether one was found. Mirrors the
// Python parse_json_from_response scan.
func extractJSONObject(s string) (string, bool) {
	// Strip markdown ```json fences first.
	s = stripMarkdownFences(s)
	// Find the first '{' that opens a balanced object.
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// fenceRE matches ```json ... ``` or ``` ... ``` markdown code fences.
var fenceRE = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// stripMarkdownFences replaces markdown code fences with their inner
// content so extractJSONObject can find the JSON object.
func stripMarkdownFences(s string) string {
	return fenceRE.ReplaceAllString(s, "$1")
}
