package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// Logout changes session state, so it is a POST behind the CSRF middleware.
// As a GET, any page the admin visited could end their session with an
// <img src="/logout">.
func TestLogoutIsNotAGet(t *testing.T) {
	r := setupRouter()
	var m mux.RouteMatch
	if r.Match(httptest.NewRequest(http.MethodGet, "/logout", nil), &m) && m.MatchErr == nil {
		t.Error("GET /logout still routes to a handler")
	}
	m = mux.RouteMatch{}
	if !r.Match(httptest.NewRequest(http.MethodPost, "/logout", nil), &m) {
		t.Error("POST /logout does not route")
	}
}
