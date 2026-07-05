package parsers

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// ZipParser extracts .zip archives into a temp directory and delegates
// each entry to the parser bound to its extension. Mirrors Python's
// parsers/zip_parser.py.
type ZipParser struct{}

func init() {
	parse.RegisterParser("zip", func() (parse.Parser, error) { return &ZipParser{}, nil })
	parse.RegisterExtension(".zip", "zip")
}

// SupportedExtensions returns .zip.
func (p *ZipParser) SupportedExtensions() []string { return []string{".zip"} }

// Parse extracts the archive into a temp directory and delegates to the
// directory parser for child routing.
func (p *ZipParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"zip parser: nil or empty local resource")
	}
	tmp, err := os.MkdirTemp("", "openviking-zip-*")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	if err := extractZip(res.Path, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	dirPR, err := (&DirectoryParser{}).Parse(ctx, &parse.LocalResource{
		Path:           tmp,
		SourceType:     parse.SourceLocal,
		OriginalSource: res.OriginalSource,
		IsTemporary:    true,
	}, opts)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	dirPR.SourceFormat = "zip"
	dirPR.ParserName = "ZipParser"
	dirPR.TempDirPath = tmp
	return dirPR, nil
}

// ParseContent parses an in-memory zip blob by writing it to a temp file
// and calling Parse.
func (p *ZipParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	tmp, err := os.CreateTemp("", "openviking-zip-input-*.zip")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	tmp.Close()
	return p.Parse(ctx, &parse.LocalResource{
		Path:           tmp.Name(),
		SourceType:     parse.SourceLocal,
		OriginalSource: sourcePath,
		IsTemporary:    false,
	}, opts)
}

// extractZip extracts src into dst, preserving the entry paths. Returns a
// wrapped domain error on failure.
func extractZip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	defer r.Close()
	for _, f := range r.File {
		if err := extractZipEntry(f, dst); err != nil {
			return err
		}
	}
	return nil
}

// extractZipEntry writes one zip entry to dst. Directories are created;
// files are written with their declared mode. Path traversal is blocked.
func extractZipEntry(f *zip.File, dst string) error {
	clean := filepath.Clean(f.Name)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("zip: unsafe entry path %q", f.Name))
	}
	out := filepath.Join(dst, clean)
	if f.FileInfo().IsDir() {
		return os.MkdirAll(out, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	rc, err := f.Open()
	if err != nil {
		return domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	defer rc.Close()
	w, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer w.Close()
	if _, err := io.Copy(w, rc); err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return nil
}
