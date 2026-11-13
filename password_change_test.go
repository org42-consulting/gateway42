package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the admin default-password guard: isDefaultPassword /
// mustChangePassword / SetMustChangePassword (auth.go) and the
// requirePasswordChanged redirect applied by the admin panel (handlers.go).

func TestIsDefaultPassword(t *testing.T) {
	// The literal, not just the constant: the login handler and the docs pin
	// this exact string as the well-known default.
	if !isDefaultPassword("admin123") {
		t.Error(`isDefaultPassword("admin123") = false, want true`)
	}
	if !isDefaultPassword(defaultAdminPassword) {
		t.Errorf("isDefaultPassword(%q) = false, want true", defaultAdminPassword)
	}
	for _, other := range []string{"", "admin12", "admin1234", "Admin123", "password", "changeme"} {
		if isDefaultPassword(other) {
			t.Errorf("isDefaultPassword(%q) = true, want false", other)
		}
	}
}

func TestMustChangePasswordFlag(t *testing.T) {
	withTestDB(t)

	// The bootstrapped admin row defaults the flag to 0: a fresh database
	// must not lock the admin out of the dashboard.
	if mustChangePassword() {
		t.Fatal("fresh database reports mustChangePassword() = true, want false")
	}

	SetMustChangePassword(true)
	if !mustChangePassword() {
		t.Fatal("mustChangePassword() = false after SetMustChangePassword(true)")
	}

	SetMustChangePassword(false)
	if mustChangePassword() {
		t.Fatal("mustChangePassword() = true after SetMustChangePassword(false)")
	}
}

// With the flag set, the admin panel must hand the admin to the settings page
// — where the change-password form lives — instead of serving the dashboard.
// The settings page itself is deliberately not guarded: it hosts the only form
// that can clear the flag, so guarding it would make the redirect loop onto
// itself with no exit. Once the flag is cleared the panel serves normally.
func TestPasswordChangeRedirectGuard(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	initTemplates()

	SetMustChangePassword(true)

	rec := httptest.NewRecorder()
	setupRouter().ServeHTTP(rec, requestWithSession(t, http.MethodGet, "/admin/panel",
		map[interface{}]interface{}{"admin": true}))
	if rec.Code != http.StatusFound {
		t.Fatalf("flag set: GET /admin/panel = %d, want 302 (body: %.300s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != changePasswordPage {
		t.Errorf("Location = %q, want %q (the change-password form's page)", loc, changePasswordPage)
	}

	// The redirect target must stay reachable while the flag is set.
	rec = httptest.NewRecorder()
	setupRouter().ServeHTTP(rec, requestWithSession(t, http.MethodGet, "/admin/settings-page",
		map[interface{}]interface{}{"admin": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("flag set: GET /admin/settings-page = %d, want 200 (guarding it would loop the redirect)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<h2>Change Admin Password</h2>`) {
		t.Error("the change-password form is missing from the settings page")
	}

	SetMustChangePassword(false)

	rec = httptest.NewRecorder()
	setupRouter().ServeHTTP(rec, requestWithSession(t, http.MethodGet, "/admin/panel",
		map[interface{}]interface{}{"admin": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("flag cleared: GET /admin/panel = %d, want 200 (body: %.300s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `<h2>Registered Clients</h2>`) {
		t.Error("the cleared-flag panel rendered no dashboard content")
	}
}
