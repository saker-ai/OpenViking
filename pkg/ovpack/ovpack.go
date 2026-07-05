// Package ovpack implements the OpenViking offline pack format.
//
// An ovpack is a ZIP archive containing a manifest, original resources,
// derived layers, vector dumps, sessions, memory, skills, and relations.
// The format is defined by docs/design/go-rewrite-design.md section 7.16.
//
// Layout:
//
//	manifest.json                       metadata + checksums
//	resources/<path>                    original file contents
//	resources/<path>.redirect.json      redirect entry for large files
//	layers/<path>.<layer>               L0/L1/L2 hidden files
//	vectors/<collection>/<name>.jsonl   per-collection vector dumps
//	sessions/<id>.json                  session snapshots
//	memory/<id>.json                    extracted memory items
//	skills/<name>.json                  skill definitions
//	relations/graph.json                relation graph
//
// Checksums use SHA-256 over the concatenation of all member file contents
// (excluding manifest.json) and over the manifest JSON with the manifest
// checksum field zeroed. The algorithm is recorded in the manifest so it
// can be swapped for xxhash64 when Python-binary compatibility is needed.
package ovpack

import (
	"errors"
)

// Format identifies the ovpack format.
const Format = "ovpack"

// Version is the ovpack schema version.
const Version = "1.0"

// Algorithm is the checksum algorithm used by this implementation.
const Algorithm = "sha256"

// ErrChecksumMismatch is returned when archive or manifest checksum
// verification fails.
var ErrChecksumMismatch = errors.New("ovpack: checksum mismatch")

// ErrRedirect is returned by ReadResource when the pack contains only a
// redirect entry for the requested URI. Use ReadResourceRedirect to
// resolve the redirect target.
var ErrRedirect = errors.New("ovpack: resource is a redirect entry")

// ErrNotFound is returned when a requested entry is not in the pack.
var ErrNotFound = errors.New("ovpack: entry not found")

// Redirect describes a large-file redirect entry stored in place of the
// original resource content. The content is fetchable from URL; Size and
// Hash let the importer verify the fetched bytes without reading the pack.
type Redirect struct {
	URL  string `json:"url"`
	Size int64  `json:"size"`
	Hash string `json:"hash,omitempty"`
}
