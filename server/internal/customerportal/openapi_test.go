package customerportal

import (
	"os"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestOpenAPIListsCustomerPortalRoutes(t *testing.T) {
	data, err := os.ReadFile("../../../shared/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	for _, path := range []string{
		"/c/v1/auth/wechat",
		"/c/v1/auth/phone/code",
		"/c/v1/auth/phone/login",
		"/c/v1/invites/resolve",
		"/c/v1/photographers/resolve",
		"/c/v1/invites/slots",
		"/c/v1/bookings",
		"/c/v1/admin/services",
	} {
		if len(document.Paths[path]) == 0 {
			t.Errorf("OpenAPI path %q is missing", path)
		}
	}

	for _, path := range []string{
		"/api/v1/contracts",
		"/api/v1/contracts/{contractID}",
		"/api/v1/contracts/{contractID}/send",
		"/api/v1/contracts/{contractID}/void",
	} {
		operations := document.Paths[path]
		if len(operations) == 0 {
			t.Errorf("OpenAPI path %q is missing", path)
			continue
		}
		for method, raw := range operations {
			operation, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			responses, ok := operation["responses"].(map[string]any)
			if !ok {
				t.Errorf("OpenAPI %s %s has no responses", method, path)
				continue
			}
			if _, ok := responses["410"]; !ok {
				t.Errorf("OpenAPI %s %s must document disabled response 410", method, path)
			}
		}
	}
}
