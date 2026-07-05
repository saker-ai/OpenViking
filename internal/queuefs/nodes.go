package queuefs

// Node type constants for the seven DAG node types per design doc 7.4.
// These are the asynq task type strings (and memory handler keys) used to
// route tasks to handlers. They follow the "queuefs:<verb>" convention.
const (
	// NodeParse parses a document into chunks. Input: resource content.
	// Output: chunks[]. Priority: critical (interactive path). Retries: 3.
	NodeParse = "queuefs:parse"

	// NodeEmbed embeds chunks into vectors. Input: chunks[]. Output:
	// vectors[]. Priority: default. Retries: 5.
	NodeEmbed = "queuefs:embed"

	// NodeUpsert upserts vectors into the vector DB. Input: vectors[].
	// Output: vectordb status. Priority: default. Retries: 5.
	NodeUpsert = "queuefs:upsert"

	// NodeExtractAbstract produces the L0 abstract. Input: resource content.
	// Output: L0 abstract. Priority: low (background understanding).
	// Retries: 3.
	NodeExtractAbstract = "queuefs:extract_abstract"

	// NodeExtractOverview produces the L1 overview. Input: chunk summaries.
	// Output: L1 overview. Priority: low. Retries: 3.
	NodeExtractOverview = "queuefs:extract_overview"

	// NodeRerank reranks retrieval candidates. Input: candidates[].
	// Output: reranked candidates. Priority: default. Retries: 5.
	NodeRerank = "queuefs:rerank"

	// NodeCommit finalizes the DAG, persisting any aggregate state.
	// Priority: default. Retries: 3.
	NodeCommit = "queuefs:commit"
)

// AllNodeTypes returns the seven DAG node types. Useful for registering fake
// handlers in tests and for validating DAG specs.
func AllNodeTypes() []string {
	return []string{
		NodeParse,
		NodeEmbed,
		NodeUpsert,
		NodeExtractAbstract,
		NodeExtractOverview,
		NodeRerank,
		NodeCommit,
	}
}

// DefaultPriority returns the design-doc default Priority for a node type.
// parse is critical (interactive); extract_abstract and extract_overview are
// low (background understanding); all others are default.
func DefaultPriority(nodeType string) Priority {
	switch nodeType {
	case NodeParse:
		return PriorityCritical
	case NodeExtractAbstract, NodeExtractOverview:
		return PriorityLow
	default:
		return PriorityDefault
	}
}

// DefaultRetries returns the design-doc default MaxRetries for a node type.
// parse / extract_abstract / extract_overview / commit use 3; embed / upsert
// / rerank use 5.
func DefaultRetries(nodeType string) int {
	switch nodeType {
	case NodeParse, NodeExtractAbstract, NodeExtractOverview, NodeCommit:
		return 3
	default:
		return 5
	}
}
