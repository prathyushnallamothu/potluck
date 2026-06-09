// Package ollamafs locates model weights inside Ollama's local store, so a
// split pipeline can reuse blobs Ollama already downloaded instead of
// re-fetching gigabytes. Ollama stores models as content-addressed blobs with
// a JSON manifest per model tag; the GGUF weights are the layer with media
// type "application/vnd.ollama.image.model".
package ollamafs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultDir returns Ollama's model store directory, honoring OLLAMA_MODELS.
func DefaultDir() string {
	if dir := os.Getenv("OLLAMA_MODELS"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ollama", "models")
}

type manifest struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"layers"`
}

// ModelBlobPath resolves a model name like "llama3.2" or "user/model:tag" to
// the GGUF blob path inside modelsDir. It returns an error if the model is
// not pulled locally.
func ModelBlobPath(modelsDir, model string) (string, error) {
	host, namespace, name, tag := splitName(model)

	manifestPath := filepath.Join(modelsDir, "manifests", host, namespace, name, tag)
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("model %q not found in ollama store: %w", model, err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("bad manifest for %q: %w", model, err)
	}
	for _, l := range m.Layers {
		if l.MediaType == "application/vnd.ollama.image.model" {
			blob := strings.Replace(l.Digest, "sha256:", "sha256-", 1)
			path := filepath.Join(modelsDir, "blobs", blob)
			if _, err := os.Stat(path); err != nil {
				return "", fmt.Errorf("manifest for %q points at missing blob %s", model, blob)
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("manifest for %q has no model layer", model)
}

// splitName breaks an Ollama model reference into its store path parts.
// Defaults mirror Ollama's: registry.ollama.ai, library namespace, latest tag.
func splitName(model string) (host, namespace, name, tag string) {
	host, namespace, tag = "registry.ollama.ai", "library", "latest"
	name = model

	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag = name[i+1:]
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	switch len(parts) {
	case 3:
		host, namespace, name = parts[0], parts[1], parts[2]
	case 2:
		namespace, name = parts[0], parts[1]
	}
	return host, namespace, name, tag
}
