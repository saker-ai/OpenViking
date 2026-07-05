package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

func TestImageParser_NoVLM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.png")
	if err := os.WriteFile(path, []byte("fake image"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := &ImageParser{}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	_, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err == nil {
		t.Fatal("Parse succeeded without VLM client, want ErrUnsupported")
	}
	var appErr *domain.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error is not *domain.AppError: %v", err)
	}
	if appErr.Code != domain.CodeUnsupported {
		t.Errorf("error code = %s, want %s", appErr.Code, domain.CodeUnsupported)
	}
}

func TestAudioParser_NoVLM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp3")
	if err := os.WriteFile(path, []byte("fake audio"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := &AudioParser{}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	_, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err == nil {
		t.Fatal("Parse succeeded without VLM client, want ErrUnsupported")
	}
	var appErr *domain.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error is not *domain.AppError: %v", err)
	}
	if appErr.Code != domain.CodeUnsupported {
		t.Errorf("error code = %s, want %s", appErr.Code, domain.CodeUnsupported)
	}
}

func TestVideoParser_NoVLM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")
	if err := os.WriteFile(path, []byte("fake video"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := &VideoParser{}
	res := &parse.LocalResource{Path: path, SourceType: parse.SourceLocal}
	_, err := p.Parse(context.Background(), res, parse.ParserOptions{})
	if err == nil {
		t.Fatal("Parse succeeded without VLM client, want ErrUnsupported")
	}
	var appErr *domain.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error is not *domain.AppError: %v", err)
	}
	if appErr.Code != domain.CodeUnsupported {
		t.Errorf("error code = %s, want %s", appErr.Code, domain.CodeUnsupported)
	}
}

func TestImageParser_ParseContent_NoVLM(t *testing.T) {
	p := &ImageParser{}
	_, err := p.ParseContent(context.Background(), []byte("fake"), "test.png", parse.ParserOptions{})
	if err == nil {
		t.Fatal("ParseContent succeeded without VLM, want ErrUnsupported")
	}
}

func TestImageParser_SupportedExtensions(t *testing.T) {
	p := &ImageParser{}
	exts := p.SupportedExtensions()
	if len(exts) == 0 {
		t.Error("SupportedExtensions returned empty slice")
	}
	found := false
	for _, e := range exts {
		if e == ".png" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf(".png not in SupportedExtensions: %v", exts)
	}
}

func TestAudioParser_SupportedExtensions(t *testing.T) {
	p := &AudioParser{}
	exts := p.SupportedExtensions()
	if len(exts) == 0 {
		t.Error("SupportedExtensions returned empty slice")
	}
}

func TestVideoParser_SupportedExtensions(t *testing.T) {
	p := &VideoParser{}
	exts := p.SupportedExtensions()
	if len(exts) == 0 {
		t.Error("SupportedExtensions returned empty slice")
	}
}

func TestMimeTypeFor(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"image.png", "image/png"},
		{"photo.jpg", "image/jpeg"},
		{"photo.jpeg", "image/jpeg"},
		{"anim.gif", "image/gif"},
		{"pic.webp", "image/webp"},
		{"icon.bmp", "image/bmp"},
		{"vector.svg", "image/svg+xml"},
		{"scan.tiff", "image/tiff"},
		{"scan.tif", "image/tiff"},
		{"song.mp3", "audio/mpeg"},
		{"voice.wav", "audio/wav"},
		{"clip.m4a", "audio/mp4"},
		{"lossless.flac", "audio/flac"},
		{"audio.ogg", "audio/ogg"},
		{"audio.aac", "audio/aac"},
		{"audio.opus", "audio/opus"},
		{"video.mp4", "video/mp4"},
		{"video.mkv", "video/x-matroska"},
		{"video.webm", "video/webm"},
		{"video.mov", "video/quicktime"},
		{"video.avi", "video/x-msvideo"},
		{"video.m4v", "video/x-m4v"},
		{"noext", "application/octet-stream"},
		{".hidden", "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := mimeTypeFor(tc.path)
			if got != tc.want {
				t.Errorf("mimeTypeFor(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestTitleFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"", ""},
		{"/tmp/test.png", "test"},
		{"file.json", "file"},
		{"/a/b.c/d.txt", "d"},
	}
	for _, tc := range cases {
		got := titleFromPath(tc.path)
		if got != tc.want {
			t.Errorf("titleFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
