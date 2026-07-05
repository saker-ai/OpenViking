package prompts

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNoTemplates is returned when no prompt template files are found
// in either the embedded archive or the on-disk override directory.
var ErrNoTemplates = errors.New("prompts: no templates found")

// embeddedTemplates embeds the 43 YAML prompt template files shipped
// with the Python reference (openviking/prompts/templates/). The Go
// binary stays self-contained: callers without an on-disk override
// directory still get the full template set.
//
// The YAML content is identical to the Python version — do not modify
// it. The embed directive picks up every .yaml file under templates/
// recursively via the all.json placeholder (build-time generated).
//
//go:embed templates
var embeddedFS embed.FS

// EmbeddedFS returns the embedded template filesystem. Callers can
// fs.WalkDir it to enumerate every shipped template, or copy the tree
// to disk for editing. Most callers should use Store instead, which
// parses the archive into Template values.
func EmbeddedFS() embed.FS { return embeddedFS }

// loadEmbedded walks the embedded templates/ tree and returns one
// Template per .yaml file, keyed by prompt ID ("<parent_dir>.<stem>").
// Files at the root of templates/ (no parent dir) use "<stem>" as
// their ID. Nested directories use the immediate parent dir name,
// matching the Python PromptManager.list_prompts convention.
func loadEmbedded() (map[string]*Template, error) {
	out := make(map[string]*Template)
	err := fs.WalkDir(embeddedFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			return nil
		}
		data, rerr := embeddedFS.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("prompts: read embedded %s: %w", path, rerr)
		}
		t, perr := parseTemplate(path, data)
		if perr != nil {
			return fmt.Errorf("prompts: parse embedded %s: %w", path, perr)
		}
		out[promptIDFromPath(path)] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoTemplates
	}
	return out, nil
}

// loadDisk walks dir and returns one Template per .yaml file, keyed
// by prompt ID. Used by Store.Load when an on-disk override directory
// is configured. The directory layout must mirror the embedded tree.
func loadDisk(dir string) (map[string]*Template, error) {
	out := make(map[string]*Template)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("prompts: read %s: %w", path, rerr)
		}
		t, perr := parseTemplate(path, data)
		if perr != nil {
			return fmt.Errorf("prompts: parse %s: %w", path, perr)
		}
		out[promptIDFromPath(path)] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoTemplates
	}
	return out, nil
}

// promptIDFromPath derives the prompt ID from a template file path.
// Mirrors Python PromptManager.list_prompts: "<immediate_parent_dir>.<stem>".
// For paths without a parent dir, returns just "<stem>".
func promptIDFromPath(path string) string {
	// Normalize to forward slashes.
	clean := filepath.ToSlash(path)
	// Strip the leading "templates/" if present (embedded paths).
	clean = strings.TrimPrefix(clean, "templates/")
	dir := filepath.Dir(clean)
	stem := strings.TrimSuffix(filepath.Base(clean), filepath.Ext(clean))
	if dir == "." || dir == "" {
		return stem
	}
	// Use only the immediate parent directory name, not the full
	// path. Matches Python's rel_path.parent.name behavior.
	parent := filepath.Base(dir)
	return parent + "." + stem
}

// listEmbedded returns the sorted list of prompt IDs in the embedded
// archive. Used for diagnostics and the embed-count verification step.
func listEmbedded() ([]string, error) {
	templates, err := loadEmbedded()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(templates))
	for id := range templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}
