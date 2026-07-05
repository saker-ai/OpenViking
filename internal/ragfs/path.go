package ragfs

import (
	"errors"
	"path"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Normalize cleans a POSIX-style ragfs path and ensures it has a leading
// slash. Empty input returns "/". Trailing slashes are preserved for
// directory paths so that sidecar hidden files resolve consistently.
//
// Percent-decoding is the caller's responsibility (per the design doc,
// vikinguri.Parse already returns a decoded path).
func Normalize(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	clean := path.Clean(p)
	if clean == "." {
		return "/"
	}
	// path.Clean strips trailing slashes; preserve one for dir-style input
	// so that hidden-file helpers like /docs/.abstract vs
	// /docs/intro.md.abstract stay unambiguous.
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	return clean
}

// Join concatenates path elements with a single slash and normalizes the
// result. It is the ragfs analogue of path.Join but always returns an
// absolute path.
func Join(elem ...string) string {
	return Normalize(path.Join(elem...))
}

// Dir returns the cleaned parent directory of p, or "/" if p is the root.
func Dir(p string) string {
	d := path.Dir(Normalize(p))
	if d == "." {
		return "/"
	}
	return d
}

// Base returns the final element of p. For the root path it returns "".
func Base(p string) string {
	n := Normalize(p)
	if n == "/" {
		return ""
	}
	return path.Base(n)
}

// HiddenSidecar returns the path of a hidden sidecar file for a resource.
//
// For a directory resource ending in "/" (e.g. "/docs/") the sidecar is
// stored inside the directory: "/docs/.abstract".
//
// For a file resource (e.g. "/docs/intro.md") the sidecar is stored as a
// sibling with the hidden suffix appended: "/docs/intro.md.abstract".
//
// hiddenName must include its leading dot (e.g. ".abstract", ".overview",
// ".chunks").
func HiddenSidecar(resourcePath, hiddenName string) string {
	p := Normalize(resourcePath)
	if strings.HasSuffix(p, "/") {
		// directory: sidecar lives inside
		return p + hiddenName
	}
	return p + hiddenName
}

// IsHidden reports whether name starts with a dot (a hidden sidecar).
func IsHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// IsNotFound reports whether err is a not-found error from ragfs or domain.
func IsNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}

// IsConflict reports whether err is a conflict / already-exists error.
func IsConflict(err error) bool {
	return errors.Is(err, domain.ErrConflict) || errors.Is(err, ErrAlreadyExists)
}
