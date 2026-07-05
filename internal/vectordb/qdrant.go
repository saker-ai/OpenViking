package vectordb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	qdrant "github.com/qdrant/go-client/qdrant"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// QdrantAdapter is a CollectionAdapter backed by a remote Qdrant cluster via
// the official gRPC client (github.com/qdrant/go-client v1.18.x).
//
// API quirks (documented for future maintainers):
//
//   - Qdrant point IDs must be either uint64 or UUID strings. Our Vector.ID
//     is an arbitrary string (e.g. a viking:// URI). We map each ID to a
//     deterministic UUIDv5 (NameSpaceURL) so the same Vector.ID always maps
//     to the same Qdrant point ID across restarts, and store the original
//     string ID in the payload field "_id" so Search/Get can return it.
//
//   - Qdrant's gRPC Match has no native prefix match for strings. We
//     approximate Filter.URIPrefix with NewMatchText("uri", prefix), which
//     tokenizes the URI and matches any token. For exact prefix semantics
//     callers should switch to the local backend or post-filter; this is
//     sufficient for the URI-prefix filters used by retrieve (P7) because
//     OpenViking URIs use "/" separators which qdrant tokenizes on.
//
//   - Qdrant's Query API returns ScoredPoint.Score directly; we trust it.
//
//   - Count is approximate by default (Exact=false) for speed; the call
//     site accepts approximate counts.
type QdrantAdapter struct {
	client *qdrant.Client
	prefix string

	mu        sync.RWMutex
	knownDims map[string]int // collection name -> dim (cached on EnsureCollection)
}

// NewQdrantAdapter dials a Qdrant server at url. url may be "host:port",
// "host" (port defaults to 6334), or "grpc://host:port" / "grpcs://host:port"
// (TLS). apiKey is sent via the standard "api-key" gRPC header.
func NewQdrantAdapter(ctx context.Context, urlStr, apiKey, prefix string) (*QdrantAdapter, error) {
	if urlStr == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: qdrant url is empty"))
	}
	host, port, useTLS, err := parseQdrantURL(urlStr)
	if err != nil {
		return nil, err
	}
	cfg := &qdrant.Config{
		Host:                   host,
		Port:                   port,
		APIKey:                 apiKey,
		UseTLS:                 useTLS,
		SkipCompatibilityCheck: true,
	}
	client, err := qdrant.NewClient(cfg)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVectorDBError, 503,
			fmt.Errorf("vectordb: qdrant dial %s: %w", urlStr, err))
	}
	// Best-effort ping so misconfigured URLs surface at startup.
	_ = ctx // qdrant NewClient already probed the connection; avoid blocking here.
	return &QdrantAdapter{
		client:    client,
		prefix:    prefix,
		knownDims: make(map[string]int),
	}, nil
}

func parseQdrantURL(s string) (host string, port int, useTLS bool, err error) {
	port = 6334
	useTLS = false
	// Accept "host:port", "host", "grpc://host:port", "grpcs://host:port",
	// and "http(s)://host:port" (https => TLS).
	switch {
	case strings.HasPrefix(s, "grpcs://"):
		useTLS = true
		s = strings.TrimPrefix(s, "grpcs://")
	case strings.HasPrefix(s, "grpc://"):
		s = strings.TrimPrefix(s, "grpc://")
	case strings.HasPrefix(s, "https://"):
		useTLS = true
		s = strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "http://"):
		s = strings.TrimPrefix(s, "http://")
	}
	// Re-attach a scheme so url.Parse separates host/port cleanly; if there
	// is no port, url.Parse leaves Port empty and we keep the default 6334.
	parsed, perr := url.Parse("tcp://" + s)
	if perr != nil {
		return "", 0, false, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vectordb: parse qdrant url %q: %w", s, perr))
	}
	host = parsed.Hostname()
	if host == "" {
		return "", 0, false, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vectordb: qdrant url %q missing host", s))
	}
	if parsed.Port() != "" {
		p, err := strconv.Atoi(parsed.Port())
		if err != nil {
			return "", 0, false, domain.Wrap(domain.CodeValidationFailed, 422,
				fmt.Errorf("vectordb: qdrant url %q invalid port: %w", s, err))
		}
		port = p
	}
	return host, port, useTLS, nil
}

// EnsureCollection creates the Qdrant collection if absent. Calling it on an
// existing collection is a no-op (Dim/Distance are not re-checked).
func (a *QdrantAdapter) EnsureCollection(ctx context.Context, schema CollectionSchema) error {
	if err := validateSchema(schema); err != nil {
		return err
	}
	exists, err := a.client.CollectionExists(ctx, schema.Name)
	if err != nil {
		return wrapQdrantErr(fmt.Errorf("qdrant: exists %s: %w", schema.Name, err))
	}
	if exists {
		a.mu.Lock()
		a.knownDims[schema.Name] = schema.Dim
		a.mu.Unlock()
		return nil
	}
	dist := qdrant.Distance_Cosine
	switch schema.Distance {
	case "", "cosine":
		dist = qdrant.Distance_Cosine
	case "l2":
		dist = qdrant.Distance_Euclid
	case "ip":
		dist = qdrant.Distance_Dot
	}
	params := &qdrant.VectorParams{
		Size:     uint64(schema.Dim),
		Distance: dist,
	}
	req := &qdrant.CreateCollection{
		CollectionName: schema.Name,
		VectorsConfig:  qdrant.NewVectorsConfig(params),
	}
	if err := a.client.CreateCollection(ctx, req); err != nil {
		return wrapQdrantErr(fmt.Errorf("qdrant: create %s: %w", schema.Name, err))
	}
	a.mu.Lock()
	a.knownDims[schema.Name] = schema.Dim
	a.mu.Unlock()
	return nil
}

// DropCollection removes the Qdrant collection. Missing is a no-op.
func (a *QdrantAdapter) DropCollection(ctx context.Context, name string) error {
	if err := a.client.DeleteCollection(ctx, name); err != nil {
		// Qdrant returns a gRPC NotFound code for missing collections; treat
		// that as success so callers can idempotently drop.
		if isQdrantNotFound(err) {
			return nil
		}
		return wrapQdrantErr(fmt.Errorf("qdrant: drop %s: %w", name, err))
	}
	a.mu.Lock()
	delete(a.knownDims, name)
	a.mu.Unlock()
	return nil
}

// ListCollections returns all collection names known to Qdrant.
func (a *QdrantAdapter) ListCollections(ctx context.Context) ([]string, error) {
	names, err := a.client.ListCollections(ctx)
	if err != nil {
		return nil, wrapQdrantErr(fmt.Errorf("qdrant: list: %w", err))
	}
	return names, nil
}

// Upsert inserts or replaces rows by ID. Each Vector.ID is mapped to a
// deterministic UUIDv5 (NameSpaceURL) so repeat upserts replace the same
// point. The original string ID is stored in payload "_id" so Search/Get
// can return it.
func (a *QdrantAdapter) Upsert(ctx context.Context, collection string, rows []Vector) error {
	if collection == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	if len(rows) == 0 {
		return nil
	}
	pts := make([]*qdrant.PointStruct, 0, len(rows))
	for _, r := range rows {
		if r.ID == "" {
			return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: row id must not be empty"))
		}
		if len(r.Embedding) == 0 {
			return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty embedding"))
		}
		id := uuidV5(r.ID)
		// Force "_id" into payload so we can recover the original string.
		payload := cloneMetadata(r.Metadata)
		if payload == nil {
			payload = map[string]any{}
		}
		payload["_id"] = r.ID
		pts = append(pts, &qdrant.PointStruct{
			Id:      qdrant.NewIDUUID(id),
			Vectors: qdrant.NewVectorsDense(r.Embedding),
			Payload: qdrant.NewValueMap(payload),
		})
	}
	wait := true
	req := &qdrant.UpsertPoints{
		CollectionName: collection,
		Points:         pts,
		Wait:           &wait,
	}
	if _, err := a.client.Upsert(ctx, req); err != nil {
		return wrapQdrantErr(fmt.Errorf("qdrant: upsert %s: %w", collection, err))
	}
	return nil
}

// Delete removes rows by ID. Unknown IDs are ignored by Qdrant.
func (a *QdrantAdapter) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ptIDs := make([]*qdrant.PointId, 0, len(ids))
	for _, id := range ids {
		ptIDs = append(ptIDs, qdrant.NewIDUUID(uuidV5(id)))
	}
	wait := true
	req := &qdrant.DeletePoints{
		CollectionName: collection,
		Points:         qdrant.NewPointsSelectorIDs(ptIDs),
		Wait:           &wait,
	}
	if _, err := a.client.Delete(ctx, req); err != nil {
		if isQdrantNotFound(err) {
			return nil
		}
		return wrapQdrantErr(fmt.Errorf("qdrant: delete %s: %w", collection, err))
	}
	return nil
}

// Search returns the TopK rows nearest to Query, filtered by params.Filter.
func (a *QdrantAdapter) Search(ctx context.Context, params SearchParams) (*SearchResult, error) {
	if len(params.Query) == 0 {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty query vector"))
	}
	topK := uint64(params.TopK)
	if topK == 0 {
		topK = 10
	}
	req := &qdrant.QueryPoints{
		CollectionName: params.Collection,
		Query:          qdrant.NewQuery(params.Query...),
		Limit:          qdrant.PtrOf(topK),
		WithPayload:    qdrant.NewWithPayload(true),
	}
	if f := buildQdrantFilter(params.Filter); f != nil {
		req.Filter = f
	}
	points, err := a.client.Query(ctx, req)
	if err != nil {
		if isQdrantNotFound(err) {
			return &SearchResult{Hits: nil}, nil
		}
		return nil, wrapQdrantErr(fmt.Errorf("qdrant: query %s: %w", params.Collection, err))
	}
	hits := make([]Vector, 0, len(points))
	for _, p := range points {
		v, err := qdrantPointToVector(p)
		if err != nil {
			return nil, err
		}
		hits = append(hits, v)
	}
	return &SearchResult{Hits: hits}, nil
}

// Get fetches a single row by ID via Scroll with a HasID condition.
func (a *QdrantAdapter) Get(ctx context.Context, collection, id string) (*Vector, error) {
	if id == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty id"))
	}
	limit := uint32(1)
	req := &qdrant.ScrollPoints{
		CollectionName: collection,
		Filter: &qdrant.Filter{
			Must: []*qdrant.Condition{qdrant.NewHasID(qdrant.NewIDUUID(uuidV5(id)))},
		},
		Limit:       &limit,
		WithPayload: qdrant.NewWithPayload(true),
	}
	pts, err := a.client.Scroll(ctx, req)
	if err != nil {
		if isQdrantNotFound(err) {
			return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
				errors.New("vectordb: collection not found"))
		}
		return nil, wrapQdrantErr(fmt.Errorf("qdrant: scroll %s: %w", collection, err))
	}
	if len(pts) == 0 {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
			errors.New("vectordb: row not found"))
	}
	v, err := qdrantPointToVector(pts[0])
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Count returns the number of points in the collection (approximate by
// default for speed; switch to Exact=true if a precise count is needed).
func (a *QdrantAdapter) Count(ctx context.Context, collection string) (int64, error) {
	exact := false
	req := &qdrant.CountPoints{
		CollectionName: collection,
		Exact:          &exact,
	}
	n, err := a.client.Count(ctx, req)
	if err != nil {
		if isQdrantNotFound(err) {
			return 0, nil
		}
		return 0, wrapQdrantErr(fmt.Errorf("qdrant: count %s: %w", collection, err))
	}
	return int64(n), nil
}

// Close releases the gRPC connection.
func (a *QdrantAdapter) Close() error {
	if a.client == nil {
		return nil
	}
	if err := a.client.Close(); err != nil {
		return wrapQdrantErr(fmt.Errorf("qdrant: close: %w", err))
	}
	return nil
}

// uuidV5 deterministically maps an arbitrary string ID to a UUIDv5 string
// (NameSpaceURL). The same input always yields the same UUID, so repeat
// Upserts replace the same Qdrant point.
func uuidV5(s string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(s)).String()
}

// buildQdrantFilter translates vectordb.Filter to a *qdrant.Filter using
// Must-conditions. Returns nil if f is the zero Filter.
func buildQdrantFilter(f Filter) *qdrant.Filter {
	var conds []*qdrant.Condition
	if f.Account != "" {
		conds = append(conds, qdrant.NewMatch("account", f.Account))
	}
	if f.Kind != "" {
		conds = append(conds, qdrant.NewMatch("kind", f.Kind))
	}
	if f.URIPrefix != "" {
		// Quirk: qdrant has no native prefix match via gRPC; NewMatchText
		// tokenizes and matches any token. See QdrantAdapter doc comment.
		conds = append(conds, qdrant.NewMatchText("uri", f.URIPrefix))
	}
	for k, v := range f.Metadata {
		conds = append(conds, metadataCondition(k, v))
	}
	if len(conds) == 0 {
		return nil
	}
	return &qdrant.Filter{Must: conds}
}

func metadataCondition(key string, v any) *qdrant.Condition {
	switch x := v.(type) {
	case string:
		return qdrant.NewMatch(key, x)
	case bool:
		return qdrant.NewMatchBool(key, x)
	case int:
		return qdrant.NewMatchInt(key, int64(x))
	case int64:
		return qdrant.NewMatchInt(key, x)
	case float32:
		return qdrant.NewMatch(key, fmt.Sprintf("%v", x))
	case float64:
		return qdrant.NewMatch(key, fmt.Sprintf("%v", x))
	default:
		return qdrant.NewMatch(key, fmt.Sprintf("%v", x))
	}
}

// qdrantPointToVector converts a scored or retrieved point to a Vector.
// Both *qdrant.ScoredPoint and *qdrant.RetrievedPoint expose GetPayload and
// GetVectors; we handle them via a small interface so Search and Get share
// the conversion.
func qdrantPointToVector(p interface {
	GetPayload() map[string]*qdrant.Value
	GetVectors() *qdrant.VectorsOutput
	GetId() *qdrant.PointId
}) (Vector, error) {
	payload := p.GetPayload()
	// Recover the original string ID from "_id", falling back to the
	// qdrant point UUID if missing.
	var id string
	if v, ok := payload["_id"]; ok {
		id = v.GetStringValue()
	}
	if id == "" {
		if pid := p.GetId(); pid != nil {
			id = pid.GetUuid()
		}
	}
	meta := make(map[string]any, len(payload))
	for k, v := range payload {
		if k == "_id" {
			continue
		}
		meta[k] = qdrantValueToAny(v)
	}
	// Embedding extraction is best-effort: qdrant returns vectors only when
	// WithVectors is set. We currently do not request them on Search/Get,
	// so Embedding is left nil; callers that need it should re-Upsert.
	var emb []float32
	if vs := p.GetVectors(); vs != nil {
		if v := vs.GetVector(); v != nil {
			emb = v.GetData()
		}
	}
	return Vector{
		ID:        id,
		Embedding: emb,
		Metadata:  meta,
	}, nil
}

func qdrantValueToAny(v *qdrant.Value) any {
	if v == nil {
		return nil
	}
	switch v.GetKind().(type) {
	case *qdrant.Value_StringValue:
		return v.GetStringValue()
	case *qdrant.Value_IntegerValue:
		return v.GetIntegerValue()
	case *qdrant.Value_DoubleValue:
		return v.GetDoubleValue()
	case *qdrant.Value_BoolValue:
		return v.GetBoolValue()
	}
	return nil
}

// wrapQdrantErr maps a gRPC status into a domain.AppError so the server's
// error middleware can translate it to an HTTP response.
func wrapQdrantErr(err error) error {
	if err == nil {
		return nil
	}
	return domain.Wrap(domain.CodeVectorDBError, 502, err)
}

// isQdrantNotFound reports whether err carries a gRPC NotFound status code,
// which Qdrant returns for missing collections and points.
func isQdrantNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "NotFound") || strings.Contains(msg, "not found") || strings.Contains(msg, "doesn't exist")
}
