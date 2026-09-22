package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

// CSRF protection for the cookie-authenticated admin UI.
//
// Scope: only session-cookie routes need this. The /v1/ API authenticates with
// an Authorization: Bearer header, which a cross-origin page cannot make the
// browser attach, so those routes are exempt — adding a token requirement there
// would break every OpenAI SDK client for no security gain.
//
// Defence is layered: SameSite=Lax on the session cookie (set in main.go) stops
// the browser sending credentials on cross-site POSTs at all, and the token
// below catches anything that slips past — older browsers, and same-site
// subdomain attacks that SameSite does not cover.

const (
	csrfSessionKey = "csrf"
	csrfFormField  = "_csrf"
	csrfHeaderName = "X-CSRF-Token"
)

// csrfToken returns this session's CSRF token, minting and persisting one on
// first use. Safe to call on every page render; it only writes a cookie when
// the token is new.
func csrfToken(w http.ResponseWriter, r *http.Request) string {
	sess := getSession(r)
	if tok, ok := sess.Values[csrfSessionKey].(string); ok && tok != "" {
		return tok
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is unrecoverable; emitting a guessable token
		// would be worse than emitting none, so fail closed. Every POST will
		// then be rejected until the process is restarted.
		slog.Error("csrf: crypto/rand failed", "err", err)
		return ""
	}
	tok := base64.RawURLEncoding.EncodeToString(b)

	sess.Values[csrfSessionKey] = tok
	if err := sess.Save(r, w); err != nil {
		slog.Error("csrf: session save", "err", err)
	}
	return tok
}

// csrfMiddleware rejects state-changing requests that do not carry the
// session's token. It must run before any handler that mutates state.
func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !csrfProtected(r) {
			next.ServeHTTP(w, r)
			return
		}

		want, _ := getSession(r).Values[csrfSessionKey].(string)
		got := r.Header.Get(csrfHeaderName)
		if got == "" {
			// ParseForm consumes the body. Every protected route here reads
			// form values anyway, and net/http caches the parsed result in
			// r.PostForm, so the handler's own ParseForm is a no-op.
			r.ParseForm()
			got = r.PostFormValue(csrfFormField)
		}

		if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			slog.Warn("csrf: rejected request", "path", r.URL.Path, "ip", clientIP(r))
			http.Error(w, "CSRF token missing or invalid — reload the page and retry.",
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfProtected reports whether a request must carry a token: state-changing
// methods on everything except the bearer-authenticated API surface.
func csrfProtected(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return !strings.HasPrefix(r.URL.Path, "/v1/")
}
