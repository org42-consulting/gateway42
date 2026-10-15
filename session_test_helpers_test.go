package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/sessions"
)

// withSessionStore installs a fresh cookie store for the duration of a test.
// main() normally creates it; tests never run main.
func withSessionStore(t *testing.T) {
	t.Helper()
	prev := sessionStore
	sessionStore = sessions.NewCookieStore(
		[]byte("0123456789abcdef0123456789abcdef"),
		[]byte("fedcba9876543210fedcba9876543210"),
	)
	t.Cleanup(func() { sessionStore = prev })
}

// requestWithSession returns a request to target carrying a session cookie
// that holds values, built the way the real store would issue it.
func requestWithSession(t *testing.T, method, target string, values map[interface{}]interface{}) *http.Request {
	t.Helper()
	seed := httptest.NewRequest(http.MethodGet, "/", nil)
	sess, err := sessionStore.Get(seed, sessionName)
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	for k, v := range values {
		sess.Values[k] = v
	}
	rr := httptest.NewRecorder()
	if err := sess.Save(seed, rr); err != nil {
		t.Fatalf("session save: %v", err)
	}

	r := httptest.NewRequest(method, target, nil)
	for _, c := range rr.Result().Cookies() {
		r.AddCookie(c)
	}
	return r
}
