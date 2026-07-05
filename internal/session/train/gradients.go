// Package train — semantic gradient types.
//
// Mirrors openviking/session/train/gradients.py: PatchSemanticGradient
// is a typed before/after memory file pair representing one update
// signal for one target policy.

package train

// PatchSemanticGradient is a patch-based semantic gradient for one
// target policy. The concrete patch text is a rendering concern owned
// by merge context providers; the gradient itself carries structured
// memory-file state.
type PatchSemanticGradient struct {
	BeforeFile   *MemoryFile
	AfterFile    MemoryFile
	BaseVersion  *int
	Rationale    string
	Links        []StoredLink
	Confidence   float64
	Metadata     map[string]any
}

// TargetName returns the name of the target policy this gradient
// updates. It infers the name from AfterFile.ExtraFields, falling
// back to the URI slug.
func (g PatchSemanticGradient) TargetName() string {
	fields := g.AfterFile.ExtraFields
	memoryType := g.AfterFile.MemoryType
	if memoryType == "" {
		if t, _ := fields["memory_type"].(string); t != "" {
			memoryType = t
		} else {
			memoryType = "experiences"
		}
	}
	if name, _ := fields["experience_name"].(string); name != "" {
		return name
	}
	if name, _ := fields["name"].(string); name != "" {
		return name
	}
	memoryTypeKey := memoryType
	if len(memoryTypeKey) > 0 && memoryTypeKey[len(memoryTypeKey)-1] == 's' {
		memoryTypeKey = memoryTypeKey[:len(memoryTypeKey)-1]
	}
	if name, _ := fields[memoryTypeKey+"_name"].(string); name != "" {
		return name
	}
	if uri := g.TargetURI(); uri != "" {
		return uriSlug(uri)
	}
	return "unknown_policy"
}

// TargetURI returns the URI of the target policy. AfterFile.URI wins;
// when empty, BeforeFile.URI is used (so deletes can still target a
// file by URI).
func (g PatchSemanticGradient) TargetURI() string {
	if g.AfterFile.URI != "" {
		return g.AfterFile.URI
	}
	if g.BeforeFile != nil {
		return g.BeforeFile.URI
	}
	return ""
}

// MemoryFile is the in-memory representation of a trainable policy
// file. Mirrors openviking.session.memory.dataclass.MemoryFile shape
// (subset; train framework only reads URI/MemoryType/ExtraFields).
type MemoryFile struct {
	URI          string
	MemoryType   string
	ExtraFields  map[string]any
	Content      string
	Metadata     map[string]any
}

// uriSlug returns the last path segment of uri, stripped of any .md
// suffix. Used as a fallback for TargetName when no explicit name is
// in the file's extra fields.
func uriSlug(uri string) string {
	if uri == "" {
		return ""
	}
	// Strip trailing slash.
	for len(uri) > 0 && uri[len(uri)-1] == '/' {
		uri = uri[:len(uri)-1]
	}
	// Find last slash.
	for i := len(uri) - 1; i >= 0; i-- {
		if uri[i] == '/' {
			uri = uri[i+1:]
			break
		}
	}
	// Strip .md suffix.
	if len(uri) > 3 && uri[len(uri)-3:] == ".md" {
		uri = uri[:len(uri)-3]
	}
	return uri
}
