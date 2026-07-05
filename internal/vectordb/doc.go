// Package vectordb provides the multi-backend vector engine used by
// OpenViking for retrieval, ingest, and session memory.
//
// The package exposes a single backend-agnostic interface, CollectionAdapter,
// together with a factory (NewAdapter) that selects between an in-memory
// brute-force index, an on-disk HNSW index (github.com/coder/hnsw), and a
// remote Qdrant cluster (github.com/qdrant/go-client) based on
// config.VectorDBConfig.
//
// All collections are tenant-scoped: the canonical name is built by
// CollectionName as "{collection_prefix}{account}__{kind}" so that a single
// physical backend safely hosts multiple accounts. Filter values (account,
// kind, uri_prefix, metadata) are passed via SearchParams.Filter and applied
// by every backend so that retrieve (P7) can call Search without knowing
// which backend is configured.
package vectordb
