package ovpack

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// fsWriter writes zip members to a target directory, creating parent
// directories as needed. It guards against zip-slip by rejecting any
// member whose path escapes the target directory.
type fsWriter struct {
	root string
}

func newFSWriter(root string) *fsWriter {
	return &fsWriter{root: filepath.Clean(root)}
}

func (w *fsWriter) write(f *zip.File) error {
	rel := filepath.FromSlash(f.Name)
	rel = filepath.Clean(rel)
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("ovpack: unsafe member path %q", f.Name)
	}
	dst := filepath.Join(w.root, rel)
	if !strings.HasPrefix(dst, w.root+string(os.PathSeparator)) && dst != w.root {
		return fmt.Errorf("ovpack: path %q escapes target dir", f.Name)
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(dst, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func (w *fsWriter) close() error { return nil }
