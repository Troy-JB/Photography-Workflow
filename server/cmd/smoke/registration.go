package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

func runRegistrationSmoke(c *client) error {
	step("self-service registration")
	email := "registration-" + uuid.NewString() + "@example.com"
	if err := c.expect(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"name": "Registration Smoke", "email": email, "password": "short", "tenant_name": "Registration Studio",
	}, http.StatusUnprocessableEntity, "validation_failed"); err != nil {
		return err
	}
	password := "registration-password"
	body := map[string]string{
		"name":        "Registration Smoke",
		"email":       email,
		"password":    password,
		"tenant_name": "Registration Studio",
	}

	status, raw, err := c.doStatus(http.MethodPost, "/api/v1/auth/register", "", body)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("registration returned %d: %s", status, string(raw))
	}
	var session struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		User         struct {
			Email string `json:"email"`
		} `json:"user"`
		Tenant struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"tenant"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return fmt.Errorf("decode registration session: %w", err)
	}
	if session.AccessToken == "" || session.RefreshToken == "" || session.User.Email != email || session.Tenant.Slug == "" ||
		session.Tenant.Name != body["tenant_name"] || session.Role != "owner" {
		return fmt.Errorf("registration returned an incomplete owner session: %+v", session)
	}

	var profile struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		Role string `json:"role"`
	}
	if err := c.do(http.MethodGet, "/api/v1/auth/me", session.AccessToken, nil, &profile); err != nil {
		return fmt.Errorf("registered session cannot access profile: %w", err)
	}
	if profile.User.Email != email || profile.Role != "owner" {
		return fmt.Errorf("registered profile mismatch: %+v", profile)
	}
	if err := c.expect(http.MethodPost, "/api/v1/auth/register", "", body, http.StatusConflict, "email_already_registered"); err != nil {
		return err
	}

	status, raw, err = c.doStatus(http.MethodPost, "/api/v1/auth/logout", "", map[string]string{"refresh_token": session.RefreshToken})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("logout returned %d: %s", status, string(raw))
	}
	if err := c.expect(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": session.RefreshToken}, http.StatusUnauthorized, "invalid_refresh_token"); err != nil {
		return err
	}
	fmt.Println("  registration created an owner session; duplicate email and revoked refresh token rejected")
	return nil
}
