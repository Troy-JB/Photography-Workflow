package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIDocumentIsWellFormed is the spec-level counterpart to
// TestDTOsMatchOpenAPI: it checks the shared contract itself, so a typo in a
// $ref or an operation without responses fails the build instead of surfacing
// as a broken generated client.
func TestOpenAPIDocumentIsWellFormed(t *testing.T) {
	path := filepath.Join("..", "..", "..", "shared", "openapi.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("openapi.yaml unavailable: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}
	if got, _ := doc["openapi"].(string); !strings.HasPrefix(got, "3.") {
		t.Fatalf("openapi version = %q", got)
	}

	for _, ref := range collectRefs(doc) {
		if !strings.HasPrefix(ref, "#/") {
			t.Errorf("external ref %q is not allowed; the spec must be self-contained", ref)
			continue
		}
		if _, ok := resolvePointer(doc, ref); !ok {
			t.Errorf("dangling $ref %q", ref)
		}
	}

	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("spec has no paths")
	}
	methods := map[string]bool{"get": true, "put": true, "post": true, "delete": true, "options": true, "head": true, "patch": true, "trace": true}
	seenOps := map[string]string{}
	for path, raw := range paths {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("path %s is not a mapping", path)
			continue
		}
		for method, op := range item {
			if !methods[strings.ToLower(method)] {
				continue
			}
			om, ok := op.(map[string]any)
			if !ok {
				t.Errorf("%s %s is not a mapping", method, path)
				continue
			}
			if responses, ok := om["responses"].(map[string]any); !ok || len(responses) == 0 {
				t.Errorf("%s %s has no responses", strings.ToUpper(method), path)
			}
			if id, ok := om["operationId"].(string); ok && id != "" {
				if prev, dup := seenOps[id]; dup {
					t.Errorf("duplicate operationId %q (%s and %s %s)", id, prev, strings.ToUpper(method), path)
				}
				seenOps[id] = strings.ToUpper(method) + " " + path
			}
		}
	}
}

func collectRefs(v any) []string {
	var out []string
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if key == "$ref" {
				if ref, ok := child.(string); ok {
					out = append(out, ref)
				}
				continue
			}
			out = append(out, collectRefs(child)...)
		}
	case []any:
		for _, child := range node {
			out = append(out, collectRefs(child)...)
		}
	}
	return out
}

func resolvePointer(doc map[string]any, ref string) (any, bool) {
	var cur any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
