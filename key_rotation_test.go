package main

import (
	"container/list"
	"testing"
	"time"
)

// Tests for API key rotation: keyIsExpired / RotateAPIKey / RevokeOldKey in
// auth.go, and the api_key_rotations fallback in getUserByAPIKey (db.go).

// Every withTestDB test starts its AUTOINCREMENT at 1, so an id:1 entry the
// user cache holds from one test's database is a different row in the next
// test's. Swap the package cache for a fresh one (restoring it on cleanup) so
// a lookup never resolves against another test's rows.
func resetUserCacheForTest(t *testing.T) {
	t.Helper()
	userCacheMu.Lock()
	prevLRU, prevIdx := userCacheLRU, userCacheIdx
	userCacheLRU = list.New()
	userCacheIdx = map[interface{}]*list.Element{}
	userCacheMu.Unlock()
	t.Cleanup(func() {
		userCacheMu.Lock()
		userCacheLRU, userCacheIdx = prevLRU, prevIdx
		userCacheMu.Unlock()
	})
}

// testUserID reads a user's ID by name through the writer pool.
func testUserID(t *testing.T, name string) int {
	t.Helper()
	var id int
	if err := db.QueryRow("SELECT id FROM users WHERE name = ?", name).Scan(&id); err != nil {
		t.Fatalf("lookup user %q: %v", name, err)
	}
	return id
}

func TestKeyIsExpired(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name      string
		expiresAt string
		want      bool
	}{
		{"empty means no expiry", "", false},
		{"future timestamp", now.Add(24 * time.Hour).Format(time.RFC3339), false},
		{"past timestamp", now.Add(-24 * time.Hour).Format(time.RFC3339), true},
		// A corrupted expiry must reject the key, not grant it forever.
		{"unparseable fails closed", "not-an-rfc3339-timestamp", true},
	}
	for _, c := range cases {
		if got := keyIsExpired(c.expiresAt); got != c.want {
			t.Errorf("%s: keyIsExpired(%q) = %v, want %v", c.name, c.expiresAt, got, c.want)
		}
	}
}

// graceHours == 0 is an immediate revocation: the old key must stop resolving
// the moment the rotation commits, and the returned key is the live one.
func TestRotateAPIKeyZeroGraceRevokesImmediately(t *testing.T) {
	withTestDB(t)
	resetUserCacheForTest(t)

	const oldKey = "raw-old-key"
	if err := createUser("rot-zero", oldKey, 10); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	uid := testUserID(t, "rot-zero")

	if u, err := getUserByAPIKey(oldKey); err != nil || u == nil || u.ID != uid {
		t.Fatalf("pre-rotation lookup = (%+v, %v), want user %d", u, err, uid)
	}

	newKey, err := RotateAPIKey(uid, 0)
	if err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}
	if newKey == "" || newKey == oldKey {
		t.Fatalf("RotateAPIKey returned %q, want a fresh non-empty key", newKey)
	}

	if u, err := getUserByAPIKey(oldKey); u != nil {
		t.Errorf("old key still resolves after a zero-grace rotation: %+v (err=%v)", u, err)
	}
	if u, err := getUserByAPIKey(newKey); err != nil || u == nil || u.ID != uid {
		t.Errorf("new key = (%+v, %v), want user %d", u, err, uid)
	}
}

// With a grace window the old key stays alive — resolvable only through the
// api_key_rotations fallback, since users.api_key already holds the new hash —
// until the rotation row's expiry passes.
func TestRotateAPIKeyGraceWindowKeepsOldKeyAlive(t *testing.T) {
	withTestDB(t)
	resetUserCacheForTest(t)

	const oldKey = "raw-grace-key"
	if err := createUser("rot-grace", oldKey, 10); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	uid := testUserID(t, "rot-grace")

	newKey, err := RotateAPIKey(uid, 24)
	if err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}

	for label, key := range map[string]string{"old": oldKey, "new": newKey} {
		u, err := getUserByAPIKey(key)
		if err != nil || u == nil || u.ID != uid {
			t.Errorf("in-grace %s key = (%+v, %v), want user %d", label, u, err, uid)
		}
	}

	// Age the window out without sleeping: move the rotation row's expiry
	// into the past.
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := db.Exec("UPDATE api_key_rotations SET expires_at = ? WHERE user_id = ?", past, uid); err != nil {
		t.Fatalf("expire rotation row: %v", err)
	}

	if u, err := getUserByAPIKey(oldKey); u != nil {
		t.Errorf("old key still resolves after its grace window: %+v (err=%v)", u, err)
	}
	if u, err := getUserByAPIKey(newKey); err != nil || u == nil || u.ID != uid {
		t.Errorf("new key after the old key's expiry = (%+v, %v), want user %d", u, err, uid)
	}
}

// RevokeOldKey stamps users.key_expires_at to now. The same check runs on the
// rotation fallback path, so an in-grace old key dies at once — and the
// current key is rejected too until a rotation resets the expiry.
func TestRevokeOldKeyCutsGraceShort(t *testing.T) {
	withTestDB(t)
	resetUserCacheForTest(t)

	const oldKey = "raw-revoke-key"
	if err := createUser("rot-revoke", oldKey, 10); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	uid := testUserID(t, "rot-revoke")

	newKey, err := RotateAPIKey(uid, 24)
	if err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}
	if u, _ := getUserByAPIKey(oldKey); u == nil {
		t.Fatal("old key did not resolve before the revoke")
	}

	if err := RevokeOldKey(uid); err != nil {
		t.Fatalf("RevokeOldKey: %v", err)
	}
	if u, _ := getUserByAPIKey(oldKey); u != nil {
		t.Errorf("in-grace old key still resolves after RevokeOldKey: %+v", u)
	}
	if u, _ := getUserByAPIKey(newKey); u != nil {
		t.Errorf("current key still resolves after RevokeOldKey: %+v", u)
	}
}

// A key issued after a revoke must work on first use: RotateAPIKey resets
// users.key_expires_at, or the fresh key would inherit the revoke's timestamp
// and be rejected by keyIsExpired on its very first lookup.
func TestRotationClearsStaleKeyExpiry(t *testing.T) {
	withTestDB(t)
	resetUserCacheForTest(t)

	const oldKey = "raw-leaked-key"
	if err := createUser("rot-clear", oldKey, 10); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	uid := testUserID(t, "rot-clear")

	if err := RevokeOldKey(uid); err != nil {
		t.Fatalf("RevokeOldKey: %v", err)
	}
	if u, _ := getUserByAPIKey(oldKey); u != nil {
		t.Fatalf("revoked key still resolves: %+v", u)
	}

	newKey, err := RotateAPIKey(uid, 0)
	if err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}

	if u, err := getUserByAPIKey(newKey); err != nil || u == nil || u.ID != uid {
		t.Fatalf("fresh key rejected on first use = (%+v, %v)", u, err)
	}

	var expiresAt string
	if err := db.QueryRow("SELECT key_expires_at FROM users WHERE id = ?", uid).Scan(&expiresAt); err != nil {
		t.Fatalf("read key_expires_at: %v", err)
	}
	if expiresAt != "" {
		t.Errorf("key_expires_at = %q after rotation, want ''", expiresAt)
	}
}
