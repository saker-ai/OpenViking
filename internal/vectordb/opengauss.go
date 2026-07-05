package vectordb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// OpenGaussAdapter is a CollectionAdapter backed by an OpenGauss (or any
// pgvector-compatible PostgreSQL) database via github.com/jackc/pgx/v5.
//
// Each collection is a table named "{schema}.vetor_{collection_prefix}{account}__{kind}"
// with columns:
//
//	id       TEXT PRIMARY KEY
//	vector   VECTOR(N)
//	uri      TEXT
//	metadata JSONB
//
// plus an ivfflat index using vector_cosine_ops. The "account" and "kind"
// fields are extracted from metadata into separate columns? No — for
// simplicity we keep them in metadata and use JSONB operators for filtering.
//
// API quirks (documented for future maintainers):
//
//   - pgvector accepts string literals like "[1,2,3]" for VECTOR values.
//     We pass vectors as such a string and cast in SQL with ::vector so
//     pgx doesn't need to know about the pgvector type.
//   - pgvector's <=> operator is cosine distance; we convert to similarity
//     score via 1 - distance to keep Search results in [-1, 1].
//   - JSONB metadata is read back via pgx's built-in JSONB decoding into
//     map[string]any. NULL metadata is treated as empty.
//   - The schema name is configurable; the table prefix "vetor_" is fixed
//     to avoid colliding with user tables. (Yes, "vetor_" not "vector_"
//     — keeps the table name clear of reserved-column confusion.)
//
// All SQL uses parameterized queries; collection names are validated by
// validateCollectionComponent (called from CollectionName) so they are safe
// to interpolate into table identifiers after the prefix.
type OpenGaussAdapter struct {
	pool   pgConn
	schema string

	mu        sync.RWMutex
	knownDims map[string]int // table name -> dim (cached on EnsureCollection)
}

// pgConn is the minimal *pgxpool.Pool surface the adapter uses. The real
// pool satisfies this via pgxPoolAdapter; tests use a stub to avoid network
// calls. Methods return plain Go types so the interface is mockable without
// implementing pgx.Row / pgx.Rows.
type pgConn interface {
	// Exec runs a statement that returns no rows.
	Exec(ctx context.Context, sql string, args ...any) error
	// QueryOne returns the single row as a map (column name -> value).
	// Returns (nil, nil) when no row matches.
	QueryOne(ctx context.Context, sql string, args ...any) (map[string]any, error)
	// QueryAll returns all matching rows as a slice of maps.
	QueryAll(ctx context.Context, sql string, args ...any) ([]map[string]any, error)
	// Close releases the underlying pool.
	Close()
}

// pgxPoolAdapter wraps *pgxpool.Pool to satisfy pgConn.
type pgxPoolAdapter struct {
	pool *pgxpool.Pool
}

func (a *pgxPoolAdapter) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := a.pool.Exec(ctx, sql, args...)
	return err
}

func (a *pgxPoolAdapter) QueryOne(ctx context.Context, sql string, args ...any) (map[string]any, error) {
	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return pgxRowToMap(rows)
}

func (a *pgxPoolAdapter) QueryAll(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		m, err := pgxRowToMap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (a *pgxPoolAdapter) Close() { a.pool.Close() }

// pgxRowToMap scans a single pgx.Row into a map keyed by column name.
// pgx.Rows satisfies the rowScanner interface used here.
func pgxRowToMap(rows pgx.Rows) (map[string]any, error) {
	fields := rows.FieldDescriptions()
	values := make([]any, len(fields))
	ptrs := make([]any, len(fields))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	m := make(map[string]any, len(fields))
	for i, f := range fields {
		m[f.Name] = values[i]
	}
	return m, nil
}

// NewOpenGaussAdapter opens a pgxpool against cfg.OpenGauss.DSN and returns
// an adapter rooted at cfg.OpenGauss.Schema (default "public"). The pool is
// lazy: pgxpool.New parses the DSN but does not dial until the first query.
func NewOpenGaussAdapter(_ context.Context, cfg config.OpenGaussConfig) (*OpenGaussAdapter, error) {
	if cfg.DSN == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: opengauss dsn is empty"))
	}
	pcfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vectordb: parse opengauss dsn: %w", err))
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVectorDBError, 503,
			fmt.Errorf("vectordb: opengauss connect: %w", err))
	}
	schema := cfg.Schema
	if schema == "" {
		schema = "public"
	}
	return &OpenGaussAdapter{
		pool:       &pgxPoolAdapter{pool: pool},
		schema:     schema,
		knownDims:  make(map[string]int),
	}, nil
}

// newOpenGaussAdapterWithConn is the test-only constructor that takes a
// pre-built pgConn (a stub in tests). It is unexported so production code
// must go through NewOpenGaussAdapter.
func newOpenGaussAdapterWithConn(schema string, conn pgConn) *OpenGaussAdapter {
	if schema == "" {
		schema = "public"
	}
	return &OpenGaussAdapter{
		pool:       conn,
		schema:     schema,
		knownDims:  make(map[string]int),
	}
}

// tableName returns the fully-qualified SQL identifier for a collection.
// The "vetor_" prefix is added before quoting so the whole table name is
// treated as a single identifier (preventing "vetor_" from being parsed
// as a column reference).
func (a *OpenGaussAdapter) tableName(collection string) string {
	return fmt.Sprintf("%s.%s", quoteIdent(a.schema), quoteIdent("vetor_"+collection))
}

// quoteIdent wraps a SQL identifier in double quotes, doubling any embedded
// double quotes. This keeps table/column names safe even when they happen
// to be SQL keywords.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// EnsureCollection creates the per-collection table + ivfflat index if
// absent. Calling it on an existing table is a no-op (Dim/Distance are not
// re-checked against the existing schema).
func (a *OpenGaussAdapter) EnsureCollection(ctx context.Context, schema CollectionSchema) error {
	if err := validateSchema(schema); err != nil {
		return err
	}
	table := a.tableName(schema.Name)
	// pgvector requires the vector dimension at column-definition time. We
	// embed schema.Dim directly; it has been validated > 0 by validateSchema.
	// The ivfflat index uses vector_cosine_ops regardless of the distance
	// metric — pgvector selects the right ops class based on the operator
	// the query uses, and we always query with <=> (cosine).
	createTable := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id       TEXT PRIMARY KEY,
			vector   VECTOR(%d) NOT NULL,
			uri      TEXT,
			metadata JSONB
		)`, table, schema.Dim)
	if err := a.pool.Exec(ctx, createTable); err != nil {
		return wrapOpenGaussErr(fmt.Errorf("opengauss: create table %s: %w", schema.Name, err))
	}
	// Idempotent index creation; lists=100 is pgvector's recommended default
	// for ~100k-row collections. For larger collections, callers should
	// build a fresh index with tuned lists; we accept the default here.
	createIndex := fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS vetor_idx_%s ON %s USING ivfflat (vector vector_cosine_ops) WITH (lists = 100)`,
		quoteIdent(schema.Name), table)
	if err := a.pool.Exec(ctx, createIndex); err != nil {
		return wrapOpenGaussErr(fmt.Errorf("opengauss: create index %s: %w", schema.Name, err))
	}
	a.mu.Lock()
	a.knownDims[schema.Name] = schema.Dim
	a.mu.Unlock()
	return nil
}

// DropCollection drops the per-collection table. Missing tables are a no-op
// (we use IF EXISTS).
func (a *OpenGaussAdapter) DropCollection(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS %s`, a.tableName(name))
	if err := a.pool.Exec(ctx, drop); err != nil {
		return wrapOpenGaussErr(fmt.Errorf("opengauss: drop %s: %w", name, err))
	}
	a.mu.Lock()
	delete(a.knownDims, name)
	a.mu.Unlock()
	return nil
}

// ListCollections queries information_schema.tables for tables matching the
// "vetor_*" prefix in the configured schema. The returned names are the
// collection names (without the "vetor_" prefix or schema qualifier).
func (a *OpenGaussAdapter) ListCollections(ctx context.Context) ([]string, error) {
	q := `SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_name LIKE 'vetor\_%'`
	rows, err := a.pool.QueryAll(ctx, q, a.schema)
	if err != nil {
		return nil, wrapOpenGaussErr(fmt.Errorf("opengauss: list: %w", err))
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		name, _ := r["table_name"].(string)
		if name == "" {
			continue
		}
		out = append(out, strings.TrimPrefix(name, "vetor_"))
	}
	return out, nil
}

// Upsert inserts or replaces rows by ID via INSERT ... ON CONFLICT.
// Vectors are passed as string literals like "[1,2,3]" and cast to VECTOR.
func (a *OpenGaussAdapter) Upsert(ctx context.Context, collection string, rows []Vector) error {
	if collection == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	if len(rows) == 0 {
		return nil
	}
	for _, r := range rows {
		if r.ID == "" {
			return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: row id must not be empty"))
		}
		if len(r.Embedding) == 0 {
			return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty embedding"))
		}
	}
	table := a.tableName(collection)
	stmt := fmt.Sprintf(
		`INSERT INTO %s (id, vector, uri, metadata)
		 VALUES ($1, $2::vector, $3, $4::jsonb)
		 ON CONFLICT (id) DO UPDATE SET
		   vector = EXCLUDED.vector,
		   uri = EXCLUDED.uri,
		   metadata = EXCLUDED.metadata`, table)
	for _, r := range rows {
		uri, _ := r.Metadata["uri"].(string)
		meta, err := json.Marshal(r.Metadata)
		if err != nil {
			return domain.Wrap(domain.CodeVectorDBError, 500,
				fmt.Errorf("opengauss: marshal metadata %s: %w", r.ID, err))
		}
		if err := a.pool.Exec(ctx, stmt, r.ID, vectorLiteral(r.Embedding), uri, meta); err != nil {
			return wrapOpenGaussErr(fmt.Errorf("opengauss: upsert %s: %w", collection, err))
		}
	}
	return nil
}

// Delete removes rows by ID. Unknown IDs are ignored.
func (a *OpenGaussAdapter) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	stmt := fmt.Sprintf(`DELETE FROM %s WHERE id = ANY($1)`, a.tableName(collection))
	if err := a.pool.Exec(ctx, stmt, ids); err != nil {
		return wrapOpenGaussErr(fmt.Errorf("opengauss: delete %s: %w", collection, err))
	}
	return nil
}

// Search returns the TopK rows nearest to Query (cosine distance), filtered
// by params.Filter. The filter is built dynamically as a WHERE clause; the
// metadata field is a JSONB column queried with the @> containment operator.
func (a *OpenGaussAdapter) Search(ctx context.Context, params SearchParams) (*SearchResult, error) {
	if len(params.Query) == 0 {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty query vector"))
	}
	topK := params.TopK
	if topK <= 0 {
		topK = 10
	}
	table := a.tableName(params.Collection)
	where, args := buildOpenGaussWhere(params.Filter)
	// args so far: filter args (1..N). Query vector goes at position N+1,
	// TopK at N+2.
	qv := vectorLiteral(params.Query)
	q := fmt.Sprintf(
		`SELECT id, uri, metadata, (1 - (vector <=> $%d::vector)) AS similarity
		 FROM %s%s
		 ORDER BY vector <=> $%d::vector
		 LIMIT $%d`,
		len(args)+1, table, where, len(args)+1, len(args)+2)
	args = append(args, qv, topK)
	rows, err := a.pool.QueryAll(ctx, q, args...)
	if err != nil {
		return nil, wrapOpenGaussErr(fmt.Errorf("opengauss: search %s: %w", params.Collection, err))
	}
	hits := make([]Vector, 0, len(rows))
	for _, r := range rows {
		v := openGaussRowToVector(r)
		if sim, ok := r["similarity"].(float64); ok {
			v.Score = float32(sim)
		} else if sim, ok := r["similarity"].(float32); ok {
			v.Score = sim
		}
		hits = append(hits, v)
	}
	return &SearchResult{Hits: hits}, nil
}

// Get fetches a single row by ID. Returns ErrNotFound when absent.
func (a *OpenGaussAdapter) Get(ctx context.Context, collection, id string) (*Vector, error) {
	if id == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty id"))
	}
	q := fmt.Sprintf(`SELECT id, uri, metadata FROM %s WHERE id = $1`, a.tableName(collection))
	row, err := a.pool.QueryOne(ctx, q, id)
	if err != nil {
		return nil, wrapOpenGaussErr(fmt.Errorf("opengauss: get %s: %w", collection, err))
	}
	if row == nil {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404, errors.New("vectordb: row not found"))
	}
	v := openGaussRowToVector(row)
	return &v, nil
}

// Count returns the number of rows in the collection.
func (a *OpenGaussAdapter) Count(ctx context.Context, collection string) (int64, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) AS n FROM %s`, a.tableName(collection))
	row, err := a.pool.QueryOne(ctx, q)
	if err != nil {
		return 0, wrapOpenGaussErr(fmt.Errorf("opengauss: count %s: %w", collection, err))
	}
	if row == nil {
		return 0, nil
	}
	switch n := row["n"].(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	}
	return 0, nil
}

// Close releases the pgxpool.
func (a *OpenGaussAdapter) Close() error {
	if a.pool == nil {
		return nil
	}
	a.pool.Close()
	return nil
}

// buildOpenGaussWhere assembles the WHERE clause + bind args for a Filter.
// Returns "" (and nil args) for the zero Filter. metadata conditions use
// JSONB @> containment so callers can match arbitrary key/value pairs.
func buildOpenGaussWhere(f Filter) (string, []any) {
	var conds []string
	args := []any{}
	if f.Account != "" {
		args = append(args, f.Account)
		conds = append(conds, fmt.Sprintf("metadata->>'account' = $%d", len(args)))
	}
	if f.Kind != "" {
		args = append(args, f.Kind)
		conds = append(conds, fmt.Sprintf("metadata->>'kind' = $%d", len(args)))
	}
	if f.URIPrefix != "" {
		args = append(args, f.URIPrefix)
		conds = append(conds, fmt.Sprintf("uri LIKE ($%d || '%%')", len(args)))
	}
	for k, v := range f.Metadata {
		// Skip account/kind/uri since they're handled above as dedicated columns.
		if k == "account" || k == "kind" || k == "uri" {
			continue
		}
		args = append(args, map[string]any{k: v})
		conds = append(conds, fmt.Sprintf("metadata @> $%d::jsonb", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// vectorLiteral serializes a float32 vector as a pgvector string literal
// like "[1.2,3.4,5.6]" suitable for casting with ::vector.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		// strconv.FormatFloat with -1 precision gives the shortest
		// round-trip-safe representation of the float32 value.
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// openGaussRowToVector converts a SELECT result map to a Vector. The
// metadata column comes back as []byte (raw JSON) or already-decoded
// map[string]any depending on pgx's codec selection; we handle both.
func openGaussRowToVector(row map[string]any) Vector {
	v := Vector{}
	if id, ok := row["id"].(string); ok {
		v.ID = id
	}
	if uri, ok := row["uri"].(string); ok {
		if v.Metadata == nil {
			v.Metadata = map[string]any{}
		}
		v.Metadata["uri"] = uri
	}
	switch m := row["metadata"].(type) {
	case map[string]any:
		if v.Metadata == nil {
			v.Metadata = map[string]any{}
		}
		for k, val := range m {
			v.Metadata[k] = val
		}
	case []byte:
		if v.Metadata == nil {
			v.Metadata = map[string]any{}
		}
		// Best-effort decode; if it fails we leave the map empty.
		_ = json.Unmarshal(m, &v.Metadata)
	case string:
		if v.Metadata == nil {
			v.Metadata = map[string]any{}
		}
		_ = json.Unmarshal([]byte(m), &v.Metadata)
	}
	return v
}

// wrapOpenGaussErr wraps a pgx error as a *domain.AppError with the
// VECTORDB_ERROR business code so the server's error middleware can
// translate it to an HTTP response.
func wrapOpenGaussErr(err error) error {
	if err == nil {
		return nil
	}
	return domain.Wrap(domain.CodeVectorDBError, 502, err)
}
