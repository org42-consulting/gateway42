package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /v1/whoami is the programmatic counterpart of the admin user list: a client
// confirms which key it is presenting and that the key is active. It is a
// /v1/ route, so the auth middleware validates the key first and the wire
// shape is the OpenAI-facing one.

// wmSeedUser creates an active user.
func wmSeedUser(t *testing.T, name, key string, rateLimit int) {
	t.Helper()
	if err := createUser(name, key, rateLimit); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if _, err := db.Exec("UPDATE users SET status='active' WHERE name=?", name); err != nil {
		t.Fatalf("activate user: %v", err)
	}
}

func wmDrive(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	setupRouter().ServeHTTP(rec, r)
	return rec
}

// A valid key reports the caller's identity with the exact field names the
// contract declares: name, status, rate_limit_rpm, key_created_at.
func TestWhoamiReportsCallerIdentity(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)
	wmSeedUser(t, "wm-user", "sk-gw-wm", 60)

	var createdAt string
	if err := dbRead.QueryRow("SELECT created_at FROM users WHERE name=?", "wm-user").Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	rec := wmDrive(t, "sk-gw-wm")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	if len(body) != 4 {
		t.Errorf("response has %d fields, want exactly 4: %s", len(body), rec.Body.String())
	}
	if body["name"] != "wm-user" {
		t.Errorf("name = %v, want wm-user", body["name"])
	}
	if body["status"] != "active" {
		t.Errorf("status = %v, want active", body["status"])
	}
	if body["rate_limit_rpm"] != float64(60) {
		t.Errorf("rate_limit_rpm = %v, want 60", body["rate_limit_rpm"])
	}
	if body["key_created_at"] != createdAt {
		t.Errorf("key_created_at = %v, want %s (the stored key creation time)", body["key_created_at"], createdAt)
	}
}

// No key at all is a 401 from the auth middleware, in the OpenAI envelope
// because /v1/whoami is not an Anthropic path.
func TestWhoamiMissingKeyIs401(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)

	rec := wmDrive(t, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d %s, want 401 with no key", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"type":"authentication_error"`) {
		t.Errorf("401 body missing authentication_error type: %s", got)
	}
}

// A key no user holds is a 401 too — the handler must never see a nil user.
func TestWhoamiInvalidKeyIs401(t *testing.T) {
	withSessionStore(t)
	withTestDB(t)

	rec := wmDrive(t, "sk-gw-wm-nope")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d %s, want 401 for an unknown key", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"type":"authentication_error"`) {
		t.Errorf("401 body missing authentication_error type: %s", got)
	}
}
