package ovpack

import "time"

// Manifest is the top-level metadata record written to manifest.json.
type Manifest struct {
	Format     string       `json:"format"`
	Version    string       `json:"version"`
	ExportedAt time.Time    `json:"exported_at"`
	Exporter   string       `json:"exporter"`
	Source     Source       `json:"source"`
	Contents   Contents     `json:"contents"`
	Embedder   EmbedderInfo `json:"embedder,omitempty"`
	VLM        VLMInfo      `json:"vlm,omitempty"`
	Checksum   Checksum     `json:"checksum"`
}

// Source records where the pack was exported from.
type Source struct {
	Account string `json:"account,omitempty"`
	BaseURI string `json:"base_uri,omitempty"`
}

// Contents tallies the items in the pack.
type Contents struct {
	Resources ResourceStats `json:"resources"`
	Layers    LayerStats    `json:"layers,omitempty"`
	Vectors   VectorStats   `json:"vectors,omitempty"`
	Sessions  CountStats    `json:"sessions,omitempty"`
	Memory    CountStats    `json:"memory,omitempty"`
	Skills    CountStats    `json:"skills,omitempty"`
	Relations RelationStats `json:"relations,omitempty"`
}

// ResourceStats counts resources and total content bytes. Redirect
// entries contribute to Count but not Bytes (their content lives outside
// the pack).
type ResourceStats struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// LayerStats counts derived hidden files by layer kind.
type LayerStats struct {
	Abstract   int `json:"abstract,omitempty"`
	Overview   int `json:"overview,omitempty"`
	FallbackL1 int `json:"fallback_l1,omitempty"`
}

// VectorStats counts vector records by collection and records the
// embedding dimension.
type VectorStats struct {
	Chunks   int `json:"chunks,omitempty"`
	Abstract int `json:"abstract,omitempty"`
	Overview int `json:"overview,omitempty"`
	Dim      int `json:"dim,omitempty"`
}

// CountStats is a generic counter for items with no extra metadata.
type CountStats struct {
	Count int `json:"count"`
}

// RelationStats counts edges in the relation graph.
type RelationStats struct {
	Edges int `json:"edges,omitempty"`
}

// EmbedderInfo records the embedding model used to produce vectors in
// the pack. Importers reject packs whose dim differs from their own.
type EmbedderInfo struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Dim      int    `json:"dim,omitempty"`
}

// VLMInfo records the vision-language model used to produce L0/L1 layers.
type VLMInfo struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Checksum carries the SHA-256 hashes of the archive and manifest.
type Checksum struct {
	Algorithm string `json:"algorithm"`
	Manifest  string `json:"manifest"`
	Archive   string `json:"archive"`
}

// Layer kinds supported by the pack.
const (
	LayerAbstract   = "abstract"
	LayerOverview   = "overview"
	LayerFallbackL1 = "fallback_l1"
)
