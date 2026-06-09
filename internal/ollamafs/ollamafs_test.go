package ollamafs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitName(t *testing.T) {
	cases := []struct {
		in                         string
		host, namespace, name, tag string
	}{
		{"llama3.2", "registry.ollama.ai", "library", "llama3.2", "latest"},
		{"llama3.2:3b", "registry.ollama.ai", "library", "llama3.2", "3b"},
		{"user/model:tag", "registry.ollama.ai", "user", "model", "tag"},
		{"hf.co/org/repo:q4", "hf.co", "org", "repo", "q4"},
	}
	for _, c := range cases {
		h, ns, n, tag := splitName(c.in)
		if h != c.host || ns != c.namespace || n != c.name || tag != c.tag {
			t.Errorf("splitName(%q) = %s/%s/%s:%s, want %s/%s/%s:%s",
				c.in, h, ns, n, tag, c.host, c.namespace, c.name, c.tag)
		}
	}
}

func TestModelBlobPath(t *testing.T) {
	dir := t.TempDir()
	blobDigest := "sha256-abc123"
	manifest := `{"layers":[
		{"mediaType":"application/vnd.ollama.image.license","digest":"sha256:lic"},
		{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:abc123"}
	]}`

	manifestDir := filepath.Join(dir, "manifests", "registry.ollama.ai", "library", "tiny")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "latest"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	blobDir := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, blobDigest), []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ModelBlobPath(dir, "tiny")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(blobDir, blobDigest)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	if _, err := ModelBlobPath(dir, "missing"); err == nil {
		t.Error("expected error for model not in store")
	}
}
