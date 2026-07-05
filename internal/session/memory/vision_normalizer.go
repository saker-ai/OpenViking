// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
)

// ImageDescriptionPrompt is the prompt sent to the VLM when describing
// an image-bearing message for later memory extraction. It mirrors
// openviking.session.memory.vision_message_normalizer.IMAGE_DESCRIPTION_PROMPT.
const ImageDescriptionPrompt = "Describe this image for later memory extraction. " +
	"Focus on durable, user-relevant details such as visible people, objects, places, " +
	"actions, text, dates, and other facts that may matter in future conversations. " +
	"Return only the description."

// MessagePart is the minimal interface a message part must satisfy for
// the vision normalizer. Concrete implementations in the bot/agent
// layer satisfy this; the memory package only needs to distinguish
// text from image parts.
//
// The Python original uses openviking.message.part.TextPart and
// openviking.message.part.ImagePart directly. Because those types are
// not yet in the Go tree, the normalizer accepts any concrete part
// that exposes its kind and payload.
type MessagePart interface {
	// IsImage reports whether this part carries an image.
	IsImage() bool
	// Text returns the part's text content, or "" when it is not a
	// text part.
	Text() string
	// ImageURL returns the part's image URL, or "" when it is not an
	// image part.
	ImageURL() string
	// ImageDetail returns the optional detail hint for the image, or
	// "" when unset.
	ImageDetail() string
}

// Message is the minimal interface a message must satisfy for the
// vision normalizer. It mirrors the parts of
// openviking.message.Message used by vision_message_normalizer.
type Message interface {
	ID() string
	Role() string
	Parts() []MessagePart
	PeerID() string
	CreatedAt() string
}

// MessageBuilder constructs a new message from a parts slice. The
// vision normalizer uses it to produce the text-only replacement for
// an image-bearing message.
type MessageBuilder func(id, role, peerID, createdAt string, parts []MessagePart) Message

// VLM is the minimal interface a vision-language model must satisfy
// for the vision normalizer. It mirrors the subset of
// openviking.models.vlm.VLM used by vision_message_normalizer.
type VLM interface {
	// GetVisionCompletionAsync sends messages to the VLM and returns
	// the textual response. The messages argument is a slice of
	// map[string]any in the OpenAI chat-completions format.
	GetVisionCompletionAsync(ctx context.Context, messages []map[string]any, thinking bool) (string, error)
}

// MessageHasImagePart reports whether any part of message is an image.
func MessageHasImagePart(message Message) bool {
	if message == nil {
		return false
	}
	for _, p := range message.Parts() {
		if p.IsImage() {
			return true
		}
	}
	return false
}

// ImagePartToOpenAIContent converts a MessagePart carrying an image
// into the OpenAI chat-completions content shape. It mirrors
// image_part_to_openai_content.
func ImagePartToOpenAIContent(part MessagePart) map[string]any {
	imageURL := map[string]any{"url": part.ImageURL()}
	if d := part.ImageDetail(); d != "" {
		imageURL["detail"] = d
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": imageURL,
	}
}

// BuildVisionDescriptionMessages constructs the OpenAI-format messages
// for asking a VLM to describe an image-bearing message. It mirrors
// build_vision_description_messages.
func BuildVisionDescriptionMessages(message Message) []map[string]any {
	content := []map[string]any{
		{"type": "text", "text": ImageDescriptionPrompt},
	}
	for _, p := range message.Parts() {
		if t := p.Text(); t != "" {
			content = append(content, map[string]any{"type": "text", "text": t})
		} else if p.IsImage() {
			content = append(content, ImagePartToOpenAIContent(p))
		}
	}
	return []map[string]any{{"role": "user", "content": content}}
}

// DescribeImageMessage asks vlm to describe an image-bearing message.
// Returns "" when vlm is nil or when the call fails. It mirrors
// describe_image_message.
func DescribeImageMessage(ctx context.Context, message Message, vlm VLM) string {
	if vlm == nil {
		return ""
	}
	resp, err := vlm.GetVisionCompletionAsync(ctx, BuildVisionDescriptionMessages(message), false)
	if err != nil || resp == "" {
		return ""
	}
	return resp
}

// TextOnlyPart is a MessagePart carrying only text. It is the
// concrete type used by ReplaceImagePartsWithDescriptions when
// rebuilding messages.
type TextOnlyPart struct{ text string }

// NewTextOnlyPart returns a TextOnlyPart.
func NewTextOnlyPart(text string) TextOnlyPart { return TextOnlyPart{text: text} }

// IsImage always returns false.
func (p TextOnlyPart) IsImage() bool { return false }

// Text returns the part's text.
func (p TextOnlyPart) Text() string { return p.text }

// ImageURL always returns "".
func (p TextOnlyPart) ImageURL() string { return "" }

// ImageDetail always returns "".
func (p TextOnlyPart) ImageDetail() string { return "" }

// ReplaceImagePartsWithDescriptions returns a new message slice where
// each image-bearing message is replaced by a text-only message that
// includes the VLM's description. It mirrors
// replace_image_parts_with_descriptions.
//
// The builder callback is required because the memory package does not
// depend on a concrete Message implementation.
func ReplaceImagePartsWithDescriptions(
	ctx context.Context,
	messages []Message,
	vlm VLM,
	builder MessageBuilder,
) []Message {
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if !MessageHasImagePart(msg) {
			out = append(out, msg)
			continue
		}
		description := DescribeImageMessage(ctx, msg, vlm)
		parts := make([]MessagePart, 0, len(msg.Parts())+1)
		for _, p := range msg.Parts() {
			if t := p.Text(); t != "" {
				parts = append(parts, NewTextOnlyPart(t))
			}
		}
		if description != "" {
			parts = append(parts, NewTextOnlyPart("[Image description]: "+description))
		}
		if len(parts) > 0 {
			out = append(out, builder(msg.ID(), msg.Role(), msg.PeerID(), msg.CreatedAt(), parts))
		}
	}
	return out
}
