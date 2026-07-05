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

// RegisterUserSettings wires /api/v1/user-settings/* — per-user settings.
//
// Settings are stored as JSON files under the caller's account prefix:
//   - root blob:    /accounts/{account}/settings.json
//   - namespace:    /accounts/{account}/settings/{namespace}.json
//
// GET /user-settings returns the root blob (or {} when absent).
// PUT /user-settings replaces the root blob.
// GET /user-settings/:namespace returns one namespace blob.
// PUT /user-settings/:namespace replaces one namespace blob.
func RegisterUserSettings(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/user-settings")
	r.GET("", getUserSettings(deps))
	r.PUT("", updateUserSettings(deps))
	r.GET("/:namespace", getUserSettingNamespace(deps))
	r.PUT("/:namespace", updateUserSettingNamespace(deps))
}

func userSettingsRoot(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "settings.json"))
	}
	return "/settings.json"
}

func userSettingsDir(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "settings"))
	}
	return "/settings"
}

func userSettingNamespacePath(c *gin.Context, namespace string) string {
	return ragfs.Normalize(path.Join(userSettingsDir(c), namespace+".json"))
}

// getUserSettings handles GET /user-settings — read the root settings blob.
func getUserSettings(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		p := userSettingsRoot(c)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"path": p, "settings": gin.H{}})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var cfg any
		if err := json.Unmarshal(buf.Bytes(), &cfg); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p, "settings": cfg})
	}
}

// userSettingsRequest is the JSON body for PUT /user-settings[/:namespace].
type userSettingsRequest struct {
	Settings map[string]any `json:"settings"`
}

// updateUserSettings handles PUT /user-settings — replace the root blob.
func updateUserSettings(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req userSettingsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		p := userSettingsRoot(c)
		payload, err := json.Marshal(req.Settings)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(payload), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"path": p})
	}
}

// getUserSettingNamespace handles GET /user-settings/:namespace — read one namespace.
func getUserSettingNamespace(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		ns := c.Param("namespace")
		p := userSettingNamespacePath(c, ns)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), p, &buf); err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"namespace": ns, "path": p, "settings": gin.H{}})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		var cfg any
		if err := json.Unmarshal(buf.Bytes(), &cfg); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"namespace": ns, "path": p, "settings": cfg})
	}
}

// updateUserSettingNamespace handles PUT /user-settings/:namespace — replace one namespace.
func updateUserSettingNamespace(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		ns := c.Param("namespace")
		var req userSettingsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		p := userSettingNamespacePath(c, ns)
		payload, err := json.Marshal(req.Settings)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeParseFailed, 422, err))
			return
		}
		if err := deps.RAGFS.Write(c.Request.Context(), p, bytes.NewReader(payload), 0o644); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"namespace": ns, "path": p})
	}
}
