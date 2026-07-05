package ovpack

import (
	"fmt"
	"path"
	"strings"

	"github.com/saker-ai/ctxhub/pkg/vikinguri"
)

// uriToResourcePath converts a viking:// resource URI to its pack path
// under resources/. The scope is recorded separately in the manifest
// source; only the kind + path components are encoded in the pack path.
//
//	viking://agent/resources/docs/intro.md  -> resources/docs/intro.md
//	viking://user_42/resources/img/logo.png -> resources/img/logo.png
func uriToResourcePath(uri string) (string, error) {
	u, err := vikinguri.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Kind != vikinguri.KindResources {
		return "", fmt.Errorf("ovpack: %q is not a resource URI", uri)
	}
	return path.Join("resources", u.Path), nil
}

// uriToLayerPath converts a resource URI + layer kind to its pack path
// under layers/. The layer kind is appended as a suffix.
//
//	viking://agent/resources/docs/readme.md, "abstract"
//	  -> layers/docs/readme.md.abstract
func uriToLayerPath(uri, layer string) (string, error) {
	u, err := vikinguri.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Kind != vikinguri.KindResources {
		return "", fmt.Errorf("ovpack: %q is not a resource URI", uri)
	}
	return path.Join("layers", u.Path) + "." + layer, nil
}

// vectorsPath builds the pack path for a vector dump under vectors/.
// Collection is one of "chunks", "abstract", "overview". Name is the
// per-resource filename (e.g. "docs/readme.md.vec.jsonl").
func vectorsPath(collection, name string) string {
	return path.Join("vectors", collection, name)
}

// sessionPath builds the pack path for a session JSON.
func sessionPath(id string) string {
	if strings.HasSuffix(id, ".json") {
		return path.Join("sessions", id)
	}
	return path.Join("sessions", id+".json")
}

// memoryPath builds the pack path for a memory item JSON.
func memoryPath(id string) string {
	if strings.HasSuffix(id, ".json") {
		return path.Join("memory", id)
	}
	return path.Join("memory", id+".json")
}

// skillPath builds the pack path for a skill JSON.
func skillPath(name string) string {
	if strings.HasSuffix(name, ".json") {
		return path.Join("skills", name)
	}
	return path.Join("skills", name+".json")
}

// redirectPath returns the pack path for a resource redirect entry.
func redirectPath(resourcePath string) string {
	return resourcePath + ".redirect.json"
}
