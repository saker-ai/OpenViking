package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterPrivacyConfigs wires /api/v1/privacy-configs/* — privacy rule CRUD.
//
// Configs are stored as JSON files under the caller's account prefix:
// /accounts/{account}/privacy_configs/{id}.json
func RegisterPrivacyConfigs(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/privacy-configs")
	r.GET("", listPrivacyConfigs(deps))
	r.POST("", createPrivacyConfig(deps))
	r.GET("/:id", getPrivacyConfig(deps))
	r.PUT("/:id", updatePrivacyConfig(deps))
	r.DELETE("/:id", deletePrivacyConfig(deps))
}

// privacyConfigDir returns the ragfs directory for the caller's privacy configs.
func privacyConfigDir(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "privacy_configs"))
	}
	return "/privacy_configs"
}

func privacyConfigPath(c *gin.Context, id string) string {
	return ragfs.Normalize(path.Join(privacyConfigDir(c), id+".json"))
}

// listPrivacyConfigs handles GET /privacy-configs — list config IDs.
func listPrivacyConfigs(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		dir := privacyConfigDir(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), dir)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"configs": []any{}, "path": dir})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			if e == nil || e.Info == nil || e.Info.IsDir {
				continue
			}
			if name := e.Info.Name; len(name) > 5 && name[len(name)-5:] == ".json" {
				ids = append(ids, name[:len(name)-5])
			}
		}
		c.JSON(http.StatusOK, gin.H{"configs": ids, "path": dir})
	}
}

// privacyConfigRequest is the JSON body for POST /privacy-configs.
type privacyConfigRequest struct {
	ID     string         `json:"id"`
	Config map[string]any `json:"config"`
}

// createPrivacyConfig handles POST /privacy-configs — create a new config.
func createPrivacyConfig(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req privacyConfigRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.ID == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "id is required"))
			return
		}
		p := privacyConfigPath(c, req.ID)
		if _, err := deps.RAGFS.Stat(c.Request.Context(), p); err == nil {
			abortWithError(c, domain.ErrConflict.WithDetail("id", req.ID))
			return
		}
		payload, err := json.Marshal(req.Config)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(payload), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"id": req.ID, "path": p})
	}
}

// getPrivacyConfig handles GET /privacy-configs/:id — read a config.
func getPrivacyConfig(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		p := privacyConfigPath(c, id)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var cfg any
		body := buf.Bytes()
		if err := json.Unmarshal(body, &cfg); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "path": p, "config": cfg})
	}
}

// updatePrivacyConfig handles PUT /privacy-configs/:id — replace a config.
func updatePrivacyConfig(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		var req privacyConfigRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		p := privacyConfigPath(c, id)
		payload, err := json.Marshal(req.Config)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(payload), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "path": p})
	}
}

// deletePrivacyConfig handles DELETE /privacy-configs/:id — remove a config.
func deletePrivacyConfig(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		id := c.Param("id")
		p := privacyConfigPath(c, id)
		if err := deps.RAGFS.Remove(c.Request.Context(), p, false); err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "deleted": true})
	}
}
