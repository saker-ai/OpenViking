package routers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterSkills wires /api/v1/skills/* — skill CRUD. Skills are stored as
// JSON documents and executed by the Agent runtime (vikingbot), not by the
// context database.
//
// Skills are stored as JSON files at
// /accounts/{account}/skills/<name>.json so the skills root is portable
// across ragfs backends and survives process restarts. Each file holds a
// single domain.Skill document.
func RegisterSkills(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/skills")
	r.GET("", listSkills(deps))
	r.POST("", createSkill(deps))
	// SDK-compat: Go SDK calls POST /skills/find with {"query":...} and
	// POST /skills/validate with a skill doc. These are registered as
	// static routes BEFORE the /*uri wildcard so gin's radix tree
	// resolves them without conflict (different HTTP methods coexist
	// with the GET/PUT/DELETE wildcard).
	r.POST("/find", findSkills(deps))
	r.POST("/validate", validateSkill(deps))
	r.GET("/*uri", getSkill(deps))
	r.PUT("/*uri", updateSkill(deps))
	r.DELETE("/*uri", deleteSkill(deps))
}

// skillPath resolves the request URI against the caller's skills root.
// Trailing ".json" is stripped so callers can use either form.
func skillPath(c *gin.Context) string {
	uri := c.Param("uri")
	if uri == "" {
		uri = "/"
	}
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	uri = strings.TrimSuffix(uri, ".json")
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "skills", uri+".json"))
	}
	return ragfs.Normalize(uri + ".json")
}

// skillsRoot returns the ragfs path for the caller's skills root.
func skillsRoot(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "skills"))
	}
	return "/skills"
}

// createSkillRequest is the JSON body for POST /skills and PUT /skills/:name.
type createSkillRequest struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Trigger     string             `json:"trigger,omitempty"`
	Level       int                `json:"level,omitempty"`
	Steps       []domain.SkillStep `json:"steps,omitempty"`
	Files       []string           `json:"files,omitempty"`
	Metadata    map[string]any     `json:"metadata,omitempty"`
	// TempFileID, when set, indicates the SDK uploaded a skill directory as
	// a zip via POST /resources/temp_upload. The handler reads the temp
	// zip, finds SKILL.md, parses its YAML frontmatter for name +
	// description, and persists a skill document. Mirrors the Python
	// server's add_skill zip-from-temp flow.
	TempFileID string `json:"temp_file_id,omitempty"`
	SourceName string `json:"source_name,omitempty"`
}

// toSkill converts the request into a domain.Skill, filling timestamps.
func (req *createSkillRequest) toSkill(uri string) *domain.Skill {
	now := time.Now().UTC()
	return &domain.Skill{
		URI:         uri,
		Name:        req.Name,
		Level:       req.Level,
		Description: req.Description,
		Trigger:     req.Trigger,
		Steps:       req.Steps,
		Files:       req.Files,
		Metadata:    req.Metadata,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// readSkillDoc reads and unmarshals a skill JSON document.
func readSkillDoc(ctx context.Context, fs ragfs.FileSystem, p string) (*domain.Skill, error) {
	var buf bytes.Buffer
	if err := fs.Read(ctx, p, &buf); err != nil {
		return nil, err
	}
	var s domain.Skill
	if err := json.Unmarshal(buf.Bytes(), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// writeSkillDoc marshals and writes a skill JSON document.
func writeSkillDoc(ctx context.Context, fs ragfs.FileSystem, p string, s *domain.Skill) error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fs.Write(ctx, p, bytes.NewReader(body), 0o644)
}

// listSkills handles GET /skills — list skill names under the skills root.
// When the root does not exist yet (fresh account), an empty list is
// returned rather than an error. Returns the standard okResponse envelope
// with result={skills:[...], total:N} so the Go SDK's ListSkills (which
// unmarshals result into map[string]any and reads result["total"]) works.
func listSkills(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		root := skillsRoot(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), root)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, okResponse(gin.H{"skills": []any{}, "total": 0, "path": root}))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		skills := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			if e == nil || e.Info == nil || e.Info.IsDir {
				continue
			}
			name := strings.TrimSuffix(e.Info.Name, ".json")
			if name == "" || strings.HasPrefix(name, ".") {
				continue
			}
			skills = append(skills, map[string]any{
				"name": name,
				"path": ragfs.Normalize(path.Join(root, e.Info.Name)),
				"size": e.Info.Size,
			})
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"skills": skills, "total": len(skills), "path": root}))
	}
}

// createSkill handles POST /skills — create a new skill document.
// The "name" in the body is authoritative; the URI is derived from it.
//
// SDK-compat: the Go SDK posts {"wait":..., "data":{...skill fields...}}
// where the actual skill fields are nested under "data". When "data" is
// present, the outer request is unwrapped and the inner document becomes
// the skill payload. Flat payloads (no "data" key) still work for direct
// callers.
//
// SDK-compat zip flow: when the SDK's AddSkill uploads a skill directory,
// attachSkillData posts {wait, timeout, telemetry, target_uri, temp_file_id}
// (no data key). The handler reads the temp zip, finds SKILL.md, parses
// its YAML frontmatter for name + description, and persists the skill.
func createSkill(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		raw, err := c.GetRawData()
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		inner := raw
		if data, ok := outer["data"]; ok && len(data) > 0 {
			inner = data
		}
		var req createSkillRequest
		if err := json.Unmarshal(inner, &req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		// SDK zip flow: temp_file_id is at the outer payload level, not
		// under "data". Pull it from the outer map when the inner request
		// didn't carry it directly.
		if req.TempFileID == "" {
			if tfid, ok := outer["temp_file_id"]; ok {
				_ = json.Unmarshal(tfid, &req.TempFileID)
			}
		}
		if req.TempFileID != "" {
			s, serr := createSkillFromTempZip(c, deps, req.TempFileID, req.Name, req.Description)
			if serr != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, serr))
				return
			}
			c.JSON(http.StatusCreated, okResponse(gin.H{"skill": s, "path": s.URI}))
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "name is required"))
			return
		}
		root := skillsRoot(c)
		p := ragfs.Normalize(path.Join(root, req.Name+".json"))
		if _, err := deps.RAGFS.Stat(c.Request.Context(), p); err == nil {
			abortWithError(c, domain.NewAppError(domain.CodeConflict, 409, "skill already exists"))
			return
		} else if !ragfs.IsNotFound(err) {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		s := req.toSkill(p)
		if err := writeSkillDoc(c.Request.Context(), deps.RAGFS, p, s); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, okResponse(gin.H{"skill": s, "path": p}))
	}
}

// createSkillFromTempZip reads a previously uploaded temp zip (uploaded via
// POST /resources/temp_upload), finds SKILL.md inside, parses its YAML
// frontmatter for name + description, and persists a skill document. The
// temp file is cleaned up best-effort after extraction. callerName and
// callerDesc, when non-empty, override the frontmatter values (the SDK's
// ValidateSkill path doesn't upload a zip so these come from the outer
// payload for the non-zip flow only).
func createSkillFromTempZip(c *gin.Context, deps *Deps, tempID, callerName, callerDesc string) (*domain.Skill, error) {
	tempPath := tempFilePath(c, tempID)
	var buf bytes.Buffer
	if err := deps.RAGFS.Read(c.Request.Context(), tempPath, &buf); err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return nil, err
	}
	var skillMD []byte
	var skillPath string
	for _, f := range zr.File {
		base := path.Base(f.Name)
		if strings.EqualFold(base, "SKILL.md") {
			rc, rerr := f.Open()
			if rerr != nil {
				return nil, rerr
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			skillMD = data
			skillPath = f.Name
			break
		}
	}
	if skillMD == nil {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422, "SKILL.md not found in zip")
	}
	name, desc := parseSkillFrontmatter(skillMD)
	if callerName != "" {
		name = callerName
	}
	if callerDesc != "" {
		desc = callerDesc
	}
	if strings.TrimSpace(name) == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422, "skill name not found in SKILL.md frontmatter")
	}
	root := skillsRoot(c)
	p := ragfs.Normalize(path.Join(root, name+".json"))
	s := &domain.Skill{
		URI:         p,
		Name:        name,
		Description: desc,
		Files:       []string{skillPath},
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := writeSkillDoc(c.Request.Context(), deps.RAGFS, p, s); err != nil {
		return nil, err
	}
	// Best-effort cleanup of the temp file.
	_ = deps.RAGFS.Remove(c.Request.Context(), tempPath, false)
	return s, nil
}

// skillMatchesQuery returns true when any query token appears (case-
// insensitive) in the skill name, where the name is split on hyphens and
// underscores so "go-sdk-smoke-1783" matches tokens {"go","sdk","smoke"}.
// An empty token list matches everything. This is a fallback for the
// Python server's semantic vector search; the Go server has no vector
// index so it uses token-level OR matching.
func skillMatchesQuery(name string, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	lower := strings.ToLower(name)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '/'
	})
	fieldSet := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		fieldSet[f] = struct{}{}
	}
	for _, tok := range tokens {
		if _, ok := fieldSet[tok]; ok {
			return true
		}
	}
	return false
}

// parseSkillFrontmatter extracts name and description from the YAML
// frontmatter block (delimited by --- lines) at the top of SKILL.md. Only
// the two fields the server needs are parsed; everything else is ignored.
// When no frontmatter is present, empty strings are returned.
func parseSkillFrontmatter(data []byte) (name, description string) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", ""
	}
	rest := strings.TrimPrefix(text, "---\n")
	rest = strings.TrimPrefix(rest, "---\r\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ""
	}
	block := rest[:end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "name:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		case strings.HasPrefix(line, "description:"):
			description = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		}
	}
	return name, description
}

// getSkill handles GET /skills/:name — read a skill document. Returns the
// standard okResponse envelope with result=<skill fields> so the Go SDK's
// GetSkill (which unmarshals result into map[string]any and reads
// result["name"], result["files"]) works without unwrapping a nested
// "skill" key.
func getSkill(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := skillPath(c)
		s, err := readSkillDoc(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.NewAppError(domain.CodeResourceNotFound, 404, "skill not found"))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(s))
	}
}

// updateSkill handles PUT /skills/:name — overwrite a skill document.
// The skill must already exist; create-then-update is the supported flow.
//
// SDK-compat: the Go SDK posts {"wait":..., "data":{...skill fields...}}
// where the actual skill fields are nested under "data". When "data" is
// present, the outer request is unwrapped. Flat payloads still work.
func updateSkill(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := skillPath(c)
		existing, err := readSkillDoc(c.Request.Context(), deps.RAGFS, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.NewAppError(domain.CodeResourceNotFound, 404, "skill not found"))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		raw, err := c.GetRawData()
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		inner := raw
		if data, ok := outer["data"]; ok && len(data) > 0 {
			inner = data
		}
		var req createSkillRequest
		if err := json.Unmarshal(inner, &req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.TempFileID == "" {
			if tfid, ok := outer["temp_file_id"]; ok {
				_ = json.Unmarshal(tfid, &req.TempFileID)
			}
		}
		// SDK zip flow: re-extract SKILL.md and overwrite the skill doc
		// with the updated frontmatter. The skill name from the URL path
		// is preserved (UpdateSkill's path parameter is authoritative).
		if req.TempFileID != "" {
			updated, serr := createSkillFromTempZip(c, deps, req.TempFileID, existing.Name, "")
			if serr != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, serr))
				return
			}
			updated.CreatedAt = existing.CreatedAt
			updated.UpdatedAt = time.Now().UTC()
			if err := writeSkillDoc(c.Request.Context(), deps.RAGFS, p, updated); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			c.JSON(http.StatusOK, okResponse(gin.H{"skill": updated, "path": p}))
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			req.Name = existing.Name
		}
		s := req.toSkill(p)
		s.CreatedAt = existing.CreatedAt
		s.UpdatedAt = time.Now().UTC()
		if err := writeSkillDoc(c.Request.Context(), deps.RAGFS, p, s); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"skill": s, "path": p}))
	}
}

// deleteSkill handles DELETE /skills/:name — remove a skill document.
func deleteSkill(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := skillPath(c)
		if err := deps.RAGFS.Remove(c.Request.Context(), p, false); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.NewAppError(domain.CodeResourceNotFound, 404, "skill not found"))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"path": p, "deleted": true}))
	}
}

// findSkills handles POST /skills/find — SDK-compat alias that searches
// skill names by substring. The Go SDK posts {"query":..., "limit":...}
// and expects result to be a map with a "total" field (the SDK reads
// result["total"]). The Python server does semantic vector search; the
// Go server falls back to token-level substring matching: every
// whitespace-separated token in the query must appear (case-insensitive)
// in the skill name. This lets "Go SDK smoke validation" match a skill
// named "go-sdk-smoke-<ts>" since all four tokens (go, sdk, smoke,
// validation) appear in the name's hyphen-separated parts.
func findSkills(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req struct {
			Query string `json:"query,omitempty"`
			Limit int    `json:"limit,omitempty"`
			TopK  int    `json:"top_k,omitempty"`
		}
		_ = c.ShouldBindJSON(&req)
		q := strings.ToLower(strings.TrimSpace(req.Query))
		tokens := strings.Fields(q)
		root := skillsRoot(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), root)
		matches := make([]map[string]any, 0)
		if err != nil {
			if !ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
				return
			}
			c.JSON(http.StatusOK, okResponse(gin.H{"skills": matches, "total": 0, "query": q}))
			return
		}
		limit := req.Limit
		if limit <= 0 {
			limit = req.TopK
		}
		if limit <= 0 {
			limit = 50
		}
		for _, e := range entries {
			if e == nil || e.Info == nil || e.Info.IsDir {
				continue
			}
			name := strings.TrimSuffix(e.Info.Name, ".json")
			if name == "" || strings.HasPrefix(name, ".") {
				continue
			}
			if !skillMatchesQuery(name, tokens) {
				continue
			}
			matches = append(matches, map[string]any{
				"name": name,
				"path": ragfs.Normalize(path.Join(root, e.Info.Name)),
				"size": e.Info.Size,
			})
			if len(matches) >= limit {
				break
			}
		}
		c.JSON(http.StatusOK, okResponse(gin.H{"skills": matches, "total": len(matches), "query": q}))
	}
}

// validateSkill handles POST /skills/validate — SDK-compat alias that
// validates a skill document's shape without persisting it. Returns
// {"valid":true} when the document parses as a domain.Skill.
//
// SDK-compat: the Go SDK posts {"strict":..., "data":{...skill fields...}}
// where the skill fields are nested under "data". When "data" is present,
// the inner document is validated. Flat payloads still work.
func validateSkill(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		inner := raw
		if data, ok := outer["data"]; ok && len(data) > 0 {
			inner = data
		}
		var req createSkillRequest
		if err := json.Unmarshal(inner, &req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		// Reuse the existing toSkill converter as the shape validator.
		_ = req.toSkill("/validate")
		c.JSON(http.StatusOK, okResponse(gin.H{"valid": true}))
	}
}
