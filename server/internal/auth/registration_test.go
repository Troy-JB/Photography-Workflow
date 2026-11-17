package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeRegistration(t *testing.T) {
	in, err := normalizeRegistration(RegisterInput{
		Name:       "  摄影师  ",
		Email:      "  Owner@Example.com ",
		Password:   "correct horse",
		TenantName: "  星光工作室  ",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if in.Name != "摄影师" || in.Email != "owner@example.com" || in.TenantName != "星光工作室" {
		t.Fatalf("unexpected normalized input: %+v", in)
	}
}

func TestNormalizeRegistrationRejectsInvalidInput(t *testing.T) {
	valid := RegisterInput{Name: "Owner", Email: "owner@example.com", Password: "password-123", TenantName: "Studio"}
	tests := []struct {
		name string
		edit func(*RegisterInput)
	}{
		{"missing name", func(in *RegisterInput) { in.Name = " " }},
		{"invalid email", func(in *RegisterInput) { in.Email = "not-an-email" }},
		{"display-name email", func(in *RegisterInput) { in.Email = "Owner <owner@example.com>" }},
		{"short password", func(in *RegisterInput) { in.Password = "short" }},
		{"long password", func(in *RegisterInput) { in.Password = strings.Repeat("x", 129) }},
		{"missing studio", func(in *RegisterInput) { in.TenantName = " " }},
		{"long name", func(in *RegisterInput) { in.Name = strings.Repeat("x", 121) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := valid
			tt.edit(&in)
			if _, err := normalizeRegistration(in); err == nil || !errors.Is(err, ErrInvalidRegistration) {
				t.Fatalf("error = %v, want ErrInvalidRegistration", err)
			}
		})
	}
}

func TestTenantSlugIsSafeAndUnique(t *testing.T) {
	a := tenantSlug("Moon & Light Studio")
	b := tenantSlug("Moon & Light Studio")
	if a == b {
		t.Fatal("generated tenant slugs must be unique")
	}
	if !strings.HasPrefix(a, "moon-light-studio-") {
		t.Fatalf("slug = %q, want readable ASCII prefix", a)
	}
	if slug := tenantSlug("星光工作室"); !strings.HasPrefix(slug, "studio-") {
		t.Fatalf("non-ASCII slug = %q, want studio fallback", slug)
	}
}
