// Package media implements the L2 parser for image / audio / video
// files. The actual transcription / description is delegated to a
// parse.VLMClient injected via ParserOptions.MediaProcessor; when no
// client is wired, parsers return domain.ErrUnsupported.
//
// The internal/models/vlm package is wired in P12; tests inject a fake.
package media

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// ImageParser parses image files (.png/.jpg/.jpeg/.gif/.webp/.bmp) by
// delegating to a VLM client. Without a client it returns
// domain.ErrUnsupported.
type ImageParser struct{}

func init() {
	parse.RegisterParser("image", func() (parse.Parser, error) { return &ImageParser{}, nil })
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg", ".tiff", ".tif"} {
		parse.RegisterExtension(ext, "image")
	}
}

// SupportedExtensions lists the image extensions handled.
func (p *ImageParser) SupportedExtensions() []string {
	return []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg", ".tiff", ".tif"}
}

// Parse calls the VLM client to describe the image.
func (p *ImageParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if opts.MediaProcessor == nil {
		return nil, domain.Wrap(domain.CodeUnsupported, 501,
			fmt.Errorf("image parser: no VLM client wired (P12); see install note in parsers/media/media.go"))
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	desc, err := opts.MediaProcessor.DescribeImage(ctx, b, mimeTypeFor(res.Path), opts.Instruction)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVLMFailed, 502, err)
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	cp, _ := writeTempText(desc)
	root.AddChild(&parse.ResourceNode{
		Type:        parse.NodeParagraph,
		ContentPath: cp,
		Meta: map[string]any{
			"text":     desc,
			"mime":     mimeTypeFor(res.Path),
			"size":     len(b),
			"vlm_mode": "image",
		},
	})
	return parse.NewParseResult(root, res.Path, "image", "ImageParser"), nil
}

// ParseContent parses an in-memory image blob.
func (p *ImageParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if opts.MediaProcessor == nil {
		return nil, domain.Wrap(domain.CodeUnsupported, 501,
			fmt.Errorf("image parser: no VLM client wired (P12)"))
	}
	desc, err := opts.MediaProcessor.DescribeImage(ctx, content, mimeTypeFor(sourcePath), opts.Instruction)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVLMFailed, 502, err)
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	cp, _ := writeTempText(desc)
	root.AddChild(&parse.ResourceNode{
		Type:        parse.NodeParagraph,
		ContentPath: cp,
		Meta:        map[string]any{"text": desc, "mime": mimeTypeFor(sourcePath), "size": len(content)},
	})
	return parse.NewParseResult(root, sourcePath, "image", "ImageParser"), nil
}

// AudioParser parses audio files (.mp3/.wav/.m4a/.flac/.ogg) by
// delegating to a VLM/STT client.
type AudioParser struct{}

func init() {
	parse.RegisterParser("audio", func() (parse.Parser, error) { return &AudioParser{}, nil })
	for _, ext := range []string{".mp3", ".wav", ".m4a", ".flac", ".ogg", ".aac", ".opus"} {
		parse.RegisterExtension(ext, "audio")
	}
}

// SupportedExtensions lists the audio extensions handled.
func (p *AudioParser) SupportedExtensions() []string {
	return []string{".mp3", ".wav", ".m4a", ".flac", ".ogg", ".aac", ".opus"}
}

// Parse calls the VLM client to transcribe the audio.
func (p *AudioParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if opts.MediaProcessor == nil {
		return nil, domain.Wrap(domain.CodeUnsupported, 501,
			fmt.Errorf("audio parser: no VLM client wired (P12)"))
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	text, err := opts.MediaProcessor.TranscribeAudio(ctx, b, mimeTypeFor(res.Path), opts.Instruction)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVLMFailed, 502, err)
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	cp, _ := writeTempText(text)
	root.AddChild(&parse.ResourceNode{
		Type:        parse.NodeParagraph,
		ContentPath: cp,
		Meta:        map[string]any{"text": text, "mime": mimeTypeFor(res.Path), "size": len(b)},
	})
	return parse.NewParseResult(root, res.Path, "audio", "AudioParser"), nil
}

// ParseContent parses an in-memory audio blob.
func (p *AudioParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if opts.MediaProcessor == nil {
		return nil, domain.Wrap(domain.CodeUnsupported, 501,
			fmt.Errorf("audio parser: no VLM client wired (P12)"))
	}
	text, err := opts.MediaProcessor.TranscribeAudio(ctx, content, mimeTypeFor(sourcePath), opts.Instruction)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVLMFailed, 502, err)
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	cp, _ := writeTempText(text)
	root.AddChild(&parse.ResourceNode{
		Type:        parse.NodeParagraph,
		ContentPath: cp,
		Meta:        map[string]any{"text": text, "mime": mimeTypeFor(sourcePath), "size": len(content)},
	})
	return parse.NewParseResult(root, sourcePath, "audio", "AudioParser"), nil
}

// VideoParser parses video files (.mp4/.mkv/.webm/.mov) by delegating to
// a VLM client.
type VideoParser struct{}

func init() {
	parse.RegisterParser("video", func() (parse.Parser, error) { return &VideoParser{}, nil })
	for _, ext := range []string{".mp4", ".mkv", ".webm", ".mov", ".avi", ".m4v"} {
		parse.RegisterExtension(ext, "video")
	}
}

// SupportedExtensions lists the video extensions handled.
func (p *VideoParser) SupportedExtensions() []string {
	return []string{".mp4", ".mkv", ".webm", ".mov", ".avi", ".m4v"}
}

// Parse calls the VLM client to describe the video.
func (p *VideoParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if opts.MediaProcessor == nil {
		return nil, domain.Wrap(domain.CodeUnsupported, 501,
			fmt.Errorf("video parser: no VLM client wired (P12)"))
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	desc, err := opts.MediaProcessor.DescribeVideo(ctx, b, mimeTypeFor(res.Path), opts.Instruction)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVLMFailed, 502, err)
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	cp, _ := writeTempText(desc)
	root.AddChild(&parse.ResourceNode{
		Type:        parse.NodeParagraph,
		ContentPath: cp,
		Meta:        map[string]any{"text": desc, "mime": mimeTypeFor(res.Path), "size": len(b)},
	})
	return parse.NewParseResult(root, res.Path, "video", "VideoParser"), nil
}

// ParseContent parses an in-memory video blob.
func (p *VideoParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if opts.MediaProcessor == nil {
		return nil, domain.Wrap(domain.CodeUnsupported, 501,
			fmt.Errorf("video parser: no VLM client wired (P12)"))
	}
	desc, err := opts.MediaProcessor.DescribeVideo(ctx, content, mimeTypeFor(sourcePath), opts.Instruction)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVLMFailed, 502, err)
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	cp, _ := writeTempText(desc)
	root.AddChild(&parse.ResourceNode{
		Type:        parse.NodeParagraph,
		ContentPath: cp,
		Meta:        map[string]any{"text": desc, "mime": mimeTypeFor(sourcePath), "size": len(content)},
	})
	return parse.NewParseResult(root, sourcePath, "video", "VideoParser"), nil
}

// mimeTypeFor returns the standard MIME type for an image/audio/video
// extension. Unknown extensions return "application/octet-stream".
func mimeTypeFor(path string) string {
	idx := strings.LastIndexByte(path, '.')
	if idx < 0 {
		return "application/octet-stream"
	}
	ext := strings.ToLower(path[idx:])
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".tiff", ".tif":
		return "image/tiff"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg":
		return "audio/ogg"
	case ".aac":
		return "audio/aac"
	case ".opus":
		return "audio/opus"
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	case ".m4v":
		return "video/x-m4v"
	}
	return "application/octet-stream"
}

// writeTempText writes content to a fresh temp file. Duplicated from
// parsers/base.go to keep the media package self-contained.
func writeTempText(content string) (string, error) {
	f, err := os.CreateTemp("", "openviking-media-*.txt")
	if err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return f.Name(), nil
}

// titleFromPath returns the file base name (without extension).
func titleFromPath(path string) string {
	if path == "" {
		return ""
	}
	base := path
	if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
		base = base[idx+1:]
	}
	if idx := strings.LastIndexByte(base, '.'); idx > 0 {
		base = base[:idx]
	}
	return base
}
