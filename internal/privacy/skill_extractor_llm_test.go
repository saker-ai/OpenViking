package privacy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubVLMClient records the last request and returns a canned response.
type stubVLMClient struct {
	lastReq    VLMChatRequest
	resp       string
	err        error
	chatCalled int
}

func (s *stubVLMClient) Chat(ctx context.Context, req VLMChatRequest) (*VLMChatResponse, error) {
	s.lastReq = req
	s.chatCalled++
	if s.err != nil {
		return nil, s.err
	}
	return &VLMChatResponse{Content: s.resp, FinishReason: "stop"}, nil
}

// stubRenderer records the last render call and returns a canned prompt.
type stubRenderer struct {
	lastSkillName   string
	lastDescription string
	lastContent     string
	prompt          string
	err             error
}

func (s *stubRenderer) Render(skillName, skillDescription, content string) (string, error) {
	s.lastSkillName = skillName
	s.lastDescription = skillDescription
	s.lastContent = content
	if s.err != nil {
		return "", s.err
	}
	if s.prompt != "" {
		return s.prompt, nil
	}
	// Default: echo the inputs so tests can assert they were passed.
	return "PROMPT(" + skillName + "|" + skillDescription + "|" + content + ")", nil
}

// TestExtractSkillPrivacyValuesWithLLM_HappyPath verifies the LLM path
// produces semantic field names from a well-formed VLM response.
func TestExtractSkillPrivacyValuesWithLLM_HappyPath(t *testing.T) {
	content := `name: my-skill
user_email: foo@bar.com
api_key: sk-abcdef0123456789abcdef0123456789
`
	client := &stubVLMClient{resp: `{"values": {"user_email": "foo@bar.com", "api_key": "sk-abcdef0123456789abcdef0123456789"}}`}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "my-skill", "test skill", content, client, renderer)
	require.NoError(t, err)
	assert.Equal(t, "my-skill", renderer.lastSkillName)
	assert.Equal(t, "test skill", renderer.lastDescription)
	assert.Equal(t, content, renderer.lastContent)
	assert.Equal(t, 1, client.chatCalled)
	// Temperature should be 0 for deterministic extraction.
	assert.Equal(t, 0.0, client.lastReq.Temperature)
	assert.True(t, client.lastReq.ResponseFormatJSON)
	assert.Len(t, res.Values, 2)
	assert.Equal(t, "foo@bar.com", res.Values["user_email"])
	assert.Equal(t, "sk-abcdef0123456789abcdef0123456789", res.Values["api_key"])
	// Sanitized content should not contain the raw PII.
	assert.NotContains(t, res.SanitizedContent, "foo@bar.com")
	assert.NotContains(t, res.SanitizedContent, "sk-abcdef0123456789abcdef0123456789")
	// Should contain the semantic placeholder names.
	assert.Contains(t, res.SanitizedContent, "{{ov_privacy:skill:my-skill:user_email}}")
	assert.Contains(t, res.SanitizedContent, "{{ov_privacy:skill:my-skill:api_key}}")
	// Round-trip via RestoreSkillContent.
	restored := RestoreSkillContent(res.SanitizedContent, "my-skill", res.Values)
	assert.Contains(t, restored, "user_email: foo@bar.com")
	assert.Contains(t, restored, "api_key: sk-abcdef0123456789abcdef0123456789")
}

// TestExtractSkillPrivacyValuesWithLLM_EmptyValues verifies that empty
// string values from the VLM (Python: "field required but no concrete
// value") are parsed correctly. The placeholderize step drops empty
// values from ReplacedValues (nothing to replace in content), so we
// verify via parseLLMExtractionResponse directly.
func TestExtractSkillPrivacyValuesWithLLM_EmptyValues(t *testing.T) {
	values, err := parseLLMExtractionResponse(`{"values": {"api_key": ""}}`)
	require.NoError(t, err)
	assert.Contains(t, values, "api_key")
	assert.Equal(t, "", values["api_key"])
}

// TestExtractSkillPrivacyValuesWithLLM_MarkdownFence verifies the
// parser tolerates ```json markdown fences around the JSON payload.
func TestExtractSkillPrivacyValuesWithLLM_MarkdownFence(t *testing.T) {
	content := "name: my-skill\nuser_email: foo@bar.com\n"
	client := &stubVLMClient{resp: "```json\n{\"values\": {\"user_email\": \"foo@bar.com\"}}\n```"}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "my-skill", "test", content, client, renderer)
	require.NoError(t, err)
	assert.Equal(t, "foo@bar.com", res.Values["user_email"])
}

// TestExtractSkillPrivacyValuesWithLLM_JSONWithProse verifies the
// parser tolerates prose around the JSON object.
func TestExtractSkillPrivacyValuesWithLLM_JSONWithProse(t *testing.T) {
	content := "name: my-skill\nuser_email: foo@bar.com\n"
	client := &stubVLMClient{resp: `Here is the extraction: {"values": {"user_email": "foo@bar.com"}} Hope this helps!`}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "my-skill", "test", content, client, renderer)
	require.NoError(t, err)
	assert.Equal(t, "foo@bar.com", res.Values["user_email"])
}

// TestExtractSkillPrivacyValuesWithLLM_NilClient returns a clear error
// rather than panicking.
func TestExtractSkillPrivacyValuesWithLLM_NilClient(t *testing.T) {
	_, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", nil, &stubRenderer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VLM client is nil")
}

// TestExtractSkillPrivacyValuesWithLLM_NilRenderer returns a clear
// error rather than panicking.
func TestExtractSkillPrivacyValuesWithLLM_NilRenderer(t *testing.T) {
	_, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", &stubVLMClient{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt renderer is nil")
}

// TestExtractSkillPrivacyValuesWithLLM_RendererError propagates the
// renderer error and does not call the VLM.
func TestExtractSkillPrivacyValuesWithLLM_RendererError(t *testing.T) {
	client := &stubVLMClient{}
	renderer := &stubRenderer{err: errors.New("template not found")}

	_, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", client, renderer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "render prompt")
	assert.Contains(t, err.Error(), "template not found")
	assert.Equal(t, 0, client.chatCalled, "VLM must not be called when renderer fails")
}

// TestExtractSkillPrivacyValuesWithLLM_VLMError propagates the VLM
// error.
func TestExtractSkillPrivacyValuesWithLLM_VLMError(t *testing.T) {
	client := &stubVLMClient{err: errors.New("503 service unavailable")}
	renderer := &stubRenderer{}

	_, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", client, renderer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vlm chat")
	assert.Contains(t, err.Error(), "503 service unavailable")
}

// TestExtractSkillPrivacyValuesWithLLM_MalformedJSON returns an error
// when the VLM response is not valid JSON.
func TestExtractSkillPrivacyValuesWithLLM_MalformedJSON(t *testing.T) {
	client := &stubVLMClient{resp: "this is not JSON at all"}
	renderer := &stubRenderer{}

	_, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", client, renderer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse vlm response")
}

// TestExtractSkillPrivacyValuesWithLLM_WrongShape verifies the parser
// tolerates JSON without a "values" field (Python: data.get("values", {})
// returns empty dict).
func TestExtractSkillPrivacyValuesWithLLM_WrongShape(t *testing.T) {
	client := &stubVLMClient{resp: `{"items": ["foo", "bar"]}`}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", client, renderer)
	require.NoError(t, err)
	assert.Empty(t, res.Values)
}

// TestExtractSkillPrivacyValuesWithLLM_NonStringValues verifies
// non-string values in the JSON are stringified (Python: str(value)).
func TestExtractSkillPrivacyValuesWithLLM_NonStringValues(t *testing.T) {
	content := "name: my-skill\nport: 8080\nenabled: true\n"
	// The VLM returns an int 8080 for port and a bool for enabled.
	// Our parser should accept these and stringify them.
	client := &stubVLMClient{resp: `{"values": {"port": 8080, "enabled": true, "ratio": 0.95}}`}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "my-skill", "test", content, client, renderer)
	require.NoError(t, err)
	// The placeholderize step only keeps fields whose value appears
	// verbatim in the content. "8080" appears in "port: 8080" so it
	// should be kept; "true"/"0.95" don't appear so they're dropped
	// from ReplacedValues. Verify via parseLLMExtractionResponse
	// directly instead.
	values, parseErr := parseLLMExtractionResponse(client.resp)
	require.NoError(t, parseErr)
	assert.Equal(t, "8080", values["port"])
	assert.Equal(t, "true", values["enabled"])
	assert.Equal(t, "0.95", values["ratio"])
	// And the integer-valued content field should round-trip.
	assert.Contains(t, res.SanitizedContent, "{{ov_privacy:skill:my-skill:port}}")
	assert.NotContains(t, res.SanitizedContent, "8080")
}

// TestExtractSkillPrivacyValuesWithLLM_NullValues verifies the parser
// tolerates {"values": null} by treating it as an empty map.
func TestExtractSkillPrivacyValuesWithLLM_NullValues(t *testing.T) {
	content := "name: my-skill\n"
	client := &stubVLMClient{resp: `{"values": null}`}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "my-skill", "test", content, client, renderer)
	require.NoError(t, err)
	assert.Empty(t, res.Values)
	assert.Equal(t, content, res.SanitizedContent)
}

// TestExtractSkillPrivacyValuesWithLLM_BracesInStrings verifies the
// JSON scanner doesn't get confused by '{' inside string values.
func TestExtractSkillPrivacyValuesWithLLM_BracesInStrings(t *testing.T) {
	// Verify via parseLLMExtractionResponse directly — the placeholderize
	// step would need the value verbatim in content, which isn't the
	// point of this test.
	values, err := parseLLMExtractionResponse(`{"values": {"template": "hello {name} world"}}`)
	require.NoError(t, err)
	assert.Equal(t, "hello {name} world", values["template"])
}

// TestParseLLMExtractionResponse_DirectTable verifies the parser
// directly across a range of inputs.
func TestParseLLMExtractionResponse_DirectTable(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "simple",
			raw:  `{"values": {"a": "1", "b": "2"}}`,
			want: map[string]string{"a": "1", "b": "2"},
		},
		{
			name: "markdown_fence",
			raw:  "```json\n{\"values\": {\"a\": \"1\"}}\n```",
			want: map[string]string{"a": "1"},
		},
		{
			name: "prose_around",
			raw:  `Sure! {"values": {"a": "1"}} There you go.`,
			want: map[string]string{"a": "1"},
		},
		{
			name: "null_values",
			raw:  `{"values": null}`,
			want: map[string]string{},
		},
		{
			name: "empty_key_dropped",
			raw:  `{"values": {"": "x", "a": "1"}}`,
			want: map[string]string{"a": "1"},
		},
		{
			name: "whitespace_key_trimmed",
			raw:  `{"values": {"  a  ": "1"}}`,
			want: map[string]string{"a": "1"},
		},
		{
			name:    "no_json",
			raw:     "no json here",
			wantErr: true,
		},
		{
			name: "wrong_shape_empty_result",
			raw:  `{"items": [1, 2]}`,
			want: map[string]string{},
		},
		{
			name: "braces_in_string",
			raw:  `{"values": {"a": "x {y} z"}}`,
			want: map[string]string{"a": "x {y} z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLLMExtractionResponse(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestExtractSkillPrivacyValuesWithLLM_SemanticVsRegexFieldNames is a
// regression guard: the LLM path produces semantic field names
// ("user_email", "api_key") while the regex path produces sequential
// names ("field_1", "field_2"). This test pins the contract.
func TestExtractSkillPrivacyValuesWithLLM_SemanticVsRegexFieldNames(t *testing.T) {
	content := `name: my-skill
user_email: foo@bar.com
api_key: sk-abcdef0123456789abcdef0123456789
`
	// LLM path: semantic names.
	llmRes, err := ExtractSkillPrivacyValuesWithLLM(
		context.Background(), "my-skill", "test", content,
		&stubVLMClient{resp: `{"values": {"user_email": "foo@bar.com", "api_key": "sk-abcdef0123456789abcdef0123456789"}}`},
		&stubRenderer{},
	)
	require.NoError(t, err)
	assert.Contains(t, llmRes.SanitizedContent, "user_email}}")
	assert.Contains(t, llmRes.SanitizedContent, "api_key}}")
	assert.NotContains(t, llmRes.SanitizedContent, "field_")

	// Regex path: sequential names.
	regexRes := ExtractSkillPrivacyValues("my-skill", "test", content)
	assert.Contains(t, regexRes.SanitizedContent, "field_1}}")
	assert.Contains(t, regexRes.SanitizedContent, "field_2}}")
	assert.NotContains(t, regexRes.SanitizedContent, "user_email}}")
}

// TestExtractSkillPrivacyValuesWithLLM_FunctionRenderer verifies a
// plain function can satisfy PromptRenderer via PromptRendererFunc.
func TestExtractSkillPrivacyValuesWithLLM_FunctionRenderer(t *testing.T) {
	renderCalled := false
	renderer := PromptRendererFunc(func(skillName, desc, content string) (string, error) {
		renderCalled = true
		return "PROMPT", nil
	})
	client := &stubVLMClient{resp: `{"values": {}}`}
	_, _ = ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", client, renderer)
	assert.True(t, renderCalled, "function renderer should be invoked")
}

// TestExtractSkillPrivacyValuesWithLLM_RequestShape verifies the
// request passed to the VLM has the expected shape (single user
// message, JSON format requested, temperature 0).
func TestExtractSkillPrivacyValuesWithLLM_RequestShape(t *testing.T) {
	client := &stubVLMClient{resp: `{"values": {}}`}
	renderer := &stubRenderer{prompt: "RENDERED_PROMPT"}
	_, _ = ExtractSkillPrivacyValuesWithLLM(context.Background(), "s", "d", "c", client, renderer)
	require.Len(t, client.lastReq.Messages, 1)
	assert.Equal(t, "user", client.lastReq.Messages[0].Role)
	assert.Equal(t, "RENDERED_PROMPT", client.lastReq.Messages[0].Content)
	assert.Equal(t, 0.0, client.lastReq.Temperature)
	assert.True(t, client.lastReq.ResponseFormatJSON)
}

// TestExtractSkillPrivacyValuesWithLLM_RoundTripWithMultipleValues
// verifies a multi-field skill round-trips through LLM extraction +
// restore.
func TestExtractSkillPrivacyValuesWithLLM_RoundTripWithMultipleValues(t *testing.T) {
	content := `name: my-skill
user_email: alice@example.com
api_key: sk-abcdef0123456789abcdef0123456789
endpoint: https://api.example.com
database: production
`
	client := &stubVLMClient{resp: `{"values": {
		"user_email": "alice@example.com",
		"api_key": "sk-abcdef0123456789abcdef0123456789",
		"endpoint": "https://api.example.com",
		"database": "production"
	}}`}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "my-skill", "test", content, client, renderer)
	require.NoError(t, err)
	// None of the raw PII should appear in the sanitized content.
	for _, secret := range []string{"alice@example.com", "sk-abcdef0123456789abcdef0123456789", "https://api.example.com", "production"} {
		assert.NotContains(t, res.SanitizedContent, secret, "raw value %q should be redacted", secret)
	}
	// All four semantic placeholders should appear.
	for _, field := range []string{"user_email", "api_key", "endpoint", "database"} {
		assert.Contains(t, res.SanitizedContent, "{{ov_privacy:skill:my-skill:"+field+"}}")
	}
	// Restore should bring back the originals.
	restored := RestoreSkillContent(res.SanitizedContent, "my-skill", res.Values)
	for _, secret := range []string{"alice@example.com", "sk-abcdef0123456789abcdef0123456789", "https://api.example.com", "production"} {
		assert.Contains(t, restored, secret)
	}
	// And the restored content should not contain any placeholders.
	assert.NotContains(t, restored, "{{ov_privacy:")
}

// TestExtractSkillPrivacyValuesWithLLM_LargeContent verifies the LLM
// path handles a moderately large skill content without truncation.
func TestExtractSkillPrivacyValuesWithLLM_LargeContent(t *testing.T) {
	// Build a content with ~50 lines, 2 of which contain PII.
	var sb strings.Builder
	sb.WriteString("name: my-skill\n")
	sb.WriteString("description: a skill with many fields\n")
	for i := 0; i < 50; i++ {
		sb.WriteString("field_" + string(rune('a'+i%26)) + ": value_" + string(rune('a'+i%26)) + "\n")
	}
	sb.WriteString("user_email: alice@example.com\n")
	sb.WriteString("api_key: sk-abcdef0123456789abcdef0123456789\n")
	content := sb.String()

	client := &stubVLMClient{resp: `{"values": {"user_email": "alice@example.com", "api_key": "sk-abcdef0123456789abcdef0123456789"}}`}
	renderer := &stubRenderer{}

	res, err := ExtractSkillPrivacyValuesWithLLM(context.Background(), "my-skill", "test", content, client, renderer)
	require.NoError(t, err)
	assert.Equal(t, content, res.OriginalContent)
	assert.NotContains(t, res.SanitizedContent, "alice@example.com")
	assert.NotContains(t, res.SanitizedContent, "sk-abcdef0123456789abcdef0123456789")
}
