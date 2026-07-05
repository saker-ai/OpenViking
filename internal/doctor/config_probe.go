package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/saker-ai/ctxhub/internal/config"
)

// probeConfig loads and validates the active config file.
//
// The probe reports one ProbeResult per section (server, vectordb, embedder,
// rerank, vlm, queuefs, ragfs) plus a top-level parse result. Sections that
// are unconfigured (no provider for embedder/rerank/vlm) report status skip;
// sections that fail validation report status fail with the validator error.
func probeConfig(ctx context.Context, path string, sections []string) []ProbeResult {
	results := []ProbeResult{}

	resolved, parseErr, parseStatus := resolveConfigPath(path)
	results = append(results, ProbeResult{
		Name:   "config.parse",
		Status: parseStatus,
		Detail: resolved,
		Error:  errString(parseErr),
	})
	if parseStatus != StatusOK {
		// Cannot load — emit skip for every section so the report still
		// lists them.
		for _, s := range sections {
			results = append(results, ProbeResult{
				Name:   "config." + s,
				Status: StatusSkip,
				Detail: "config not loaded",
			})
		}
		return results
	}

	cfg, err := config.Load(resolved)
	if err != nil {
		// Distinguish validation errors (still want per-section output)
		// from hard parse errors (covered by config.parse above).
		var verrs validator.ValidationErrors
		if !errors.As(err, &verrs) {
			results = append(results, ProbeResult{
				Name:   "config.validate",
				Status: StatusFail,
				Error:  err.Error(),
			})
			for _, s := range sections {
				results = append(results, ProbeResult{
					Name:   "config." + s,
					Status: StatusSkip,
					Detail: "config validation failed",
				})
			}
			return results
		}
		// cfg is nil on validation failure. Map each validator field
		// error to a section so the report pinpoints the failure; other
		// sections are reported as skip because we cannot validate them
		// without the top-level struct.
		failedSections := validationFailedSections(verrs)
		results = append(results, ProbeResult{
			Name:   "config.validate",
			Status: StatusFail,
			Error:  truncateErr(err.Error()),
		})
		for _, s := range sections {
			if _, ok := failedSections[s]; ok {
				results = append(results, ProbeResult{
					Name:   "config." + s,
					Status: StatusFail,
					Detail: s + " validation failed",
					Error:  truncateErr(verrs.Error()),
				})
			} else {
				results = append(results, ProbeResult{
					Name:   "config." + s,
					Status: StatusSkip,
					Detail: "config validation failed",
				})
			}
		}
		return results
	}

	// If validation fully passed, cfg is non-nil. Otherwise cfg may still
	// be nil; in that case we skip per-section checks.
	if cfg == nil {
		for _, s := range sections {
			results = append(results, ProbeResult{
				Name:   "config." + s,
				Status: StatusSkip,
				Detail: "config not loaded",
			})
		}
		return results
	}

	for _, s := range sections {
		r := validateSection(cfg, s)
		results = append(results, r)
	}
	return results
}

// resolveConfigPath mirrors config.candidatePaths but reports the first
// existing candidate. Returns ("", error, StatusFail) when no candidate
// exists.
func resolveConfigPath(override string) (string, error, ProbeStatus) {
	for _, p := range configCandidatePaths(override) {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p, nil, StatusOK
		}
	}
	return "", fmt.Errorf("no config file found in search paths"), StatusFail
}

// configCandidatePaths mirrors internal/config.candidatePaths so the doctor
// reports the same resolution order the server uses.
func configCandidatePaths(override string) []string {
	paths := []string{"/etc/openviking/ov.conf"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "openviking", "ov.conf"))
	}
	if env := os.Getenv("OV_CONFIG_PATH"); env != "" {
		paths = append(paths, env)
	}
	if override != "" {
		paths = append(paths, override)
	}
	return paths
}

// validateSection runs validator on a single sub-struct so the doctor can
// pinpoint which section failed.
func validateSection(cfg *config.Config, section string) ProbeResult {
	v := validator.New()
	var (
		target any
		label  string
	)
	switch section {
	case "server":
		target, label = cfg.Server, "server"
	case "vectordb":
		target, label = cfg.VectorDB, "vectordb"
	case "embedder":
		target, label = cfg.Embedder, "embedder"
	case "rerank":
		target, label = cfg.Rerank, "rerank"
	case "vlm":
		target, label = cfg.VLM, "vlm"
	case "queuefs":
		target, label = cfg.Queue, "queue"
	case "ragfs":
		target, label = cfg.RAGFS, "ragfs"
	default:
		return ProbeResult{Name: "config." + section, Status: StatusSkip, Detail: "unknown section"}
	}
	if err := v.Struct(target); err != nil {
		return ProbeResult{
			Name:   "config." + section,
			Status: StatusFail,
			Detail: label + " validation failed",
			Error:  truncateErr(err.Error()),
		}
	}
	// Provider-gated sections: report skip when no provider is configured.
	switch section {
	case "embedder":
		if cfg.Embedder.Provider == "" {
			return ProbeResult{Name: "config.embedder", Status: StatusSkip, Detail: "no embedder provider configured"}
		}
	case "rerank":
		if cfg.Rerank.Provider == "" {
			return ProbeResult{Name: "config.rerank", Status: StatusSkip, Detail: "no rerank provider configured"}
		}
	case "vlm":
		if cfg.VLM.Provider == "" {
			return ProbeResult{Name: "config.vlm", Status: StatusSkip, Detail: "no vlm provider configured"}
		}
	case "queuefs":
		if cfg.Queue.Backend != "redis" {
			return ProbeResult{Name: "config.queuefs", Status: StatusSkip, Detail: "queue backend " + cfg.Queue.Backend + " (no remote probe)"}
		}
	}
	return ProbeResult{Name: "config." + section, Status: StatusOK, Detail: label + " valid"}
}

// validationFailedSections maps validator FieldErrors to the section names
// the doctor reports on. The validator Namespace() looks like
// "Config.VectorDB.Backend"; we lowercase the first segment after "Config."
// to match the section keys (server, vectordb, embedder, ...). "queuefs"
// is reported when the failure is in Config.Queue.
func validationFailedSections(verrs validator.ValidationErrors) map[string]struct{} {
	out := map[string]struct{}{}
	for _, fe := range verrs {
		ns := fe.Namespace()
		// Trim leading "Config.".
		seg := strings.TrimPrefix(ns, "Config.")
		if seg == ns {
			continue
		}
		// Take everything up to the first dot.
		if i := strings.Index(seg, "."); i >= 0 {
			seg = seg[:i]
		}
		seg = strings.ToLower(seg)
		switch seg {
		case "queue":
			out["queuefs"] = struct{}{}
		case "ragfs", "server", "vectordb", "embedder", "rerank", "vlm":
			out[seg] = struct{}{}
		}
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncateErr(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// joinSections is a small helper for stable section ordering.
func joinSections(parts []string) string { return strings.Join(parts, ", ") }
