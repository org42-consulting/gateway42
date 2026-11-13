package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// clientIP decides what lands in the request log's client_ip column. With no
// trusted proxies configured it keeps the legacy behaviour (the first
// X-Forwarded-For entry wins, spoofable); with a trusted set it walks the
// chain from the closest hop outward and returns the first entry not covered
// by the set, so a middle hop cannot launder a spoofed left-side entry.
// It is log metadata only, never an authorization input — and a near-pure
// function of the request plus cfg.TrustedProxies, so it is tested directly.

// tpWithProxies sets cfg.TrustedProxies for the duration of a test.
func tpWithProxies(t *testing.T, proxies ...string) {
	t.Helper()
	prev := cfg.TrustedProxies
	cfg.TrustedProxies = proxies
	t.Cleanup(func() { cfg.TrustedProxies = prev })
}

func tpRequest(xff, remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	r.RemoteAddr = remoteAddr
	return r
}

// No trusted set: the first XFF entry wins, whatever it claims (legacy).
func TestClientIPWithoutTrustedProxiesUsesFirstXFFEntry(t *testing.T) {
	tpWithProxies(t)
	if got := clientIP(tpRequest("1.2.3.4, 10.0.0.1", "203.0.113.9:5555")); got != "1.2.3.4" {
		t.Errorf("clientIP = %q, want 1.2.3.4 (first XFF entry, legacy behaviour)", got)
	}
}

// With a trusted set the chain is walked right to left: entries appended by
// trusted proxies are believed and skipped, and the first untrusted entry is
// the client. A left-to-right walk would answer the leftmost entry instead.
func TestClientIPWalksTrustedChainRightToLeft(t *testing.T) {
	cases := []struct {
		name string
		xff  string
		tr   []string
		want string
	}{
		{"trusted tail skipped", "1.2.3.4, 10.0.0.1", []string{"10.0.0.1"}, "1.2.3.4"},
		{"two trusted hops skipped", "9.9.9.9, 8.8.8.8, 10.0.0.2, 10.0.0.1",
			[]string{"10.0.0.1", "10.0.0.2"}, "8.8.8.8"},
		{"untrusted middle hop is the client", "9.9.9.9, 7.7.7.7, 10.0.0.1",
			[]string{"10.0.0.1"}, "7.7.7.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tpWithProxies(t, c.tr...)
			if got := clientIP(tpRequest(c.xff, "203.0.113.9:5555")); got != c.want {
				t.Errorf("clientIP = %q, want %q for XFF %q with trusted %v", got, c.want, c.xff, c.tr)
			}
		})
	}
}

// Every XFF entry covered by the trusted set adds nothing beyond what the
// socket knows, so the answer falls back to the connection's peer address.
func TestClientIPFallsBackToSocketWhenAllTrusted(t *testing.T) {
	tpWithProxies(t, "10.0.0.1", "10.0.0.2")
	if got := clientIP(tpRequest("10.0.0.1, 10.0.0.2", "203.0.113.9:5555")); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want 203.0.113.9 (the socket peer)", got)
	}
	// IPv6 socket address: SplitHostPort strips the brackets.
	if got := clientIP(tpRequest("", "[::1]:6379")); got != "::1" {
		t.Errorf("clientIP = %q, want ::1", got)
	}
}

// No X-Forwarded-For at all: the socket address, trusted set or not.
func TestClientIPWithoutForwardedHeaderUsesSocket(t *testing.T) {
	tpWithProxies(t, "10.0.0.0/8")
	if got := clientIP(tpRequest("", "198.51.100.7:443")); got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want 198.51.100.7", got)
	}
	tpWithProxies(t)
	if got := clientIP(tpRequest("", "198.51.100.7:443")); got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want 198.51.100.7 without a trusted set", got)
	}
}

// A trusted CIDR matches every contained IP, so a proxy anywhere in the range
// can be believed — and an IP just outside the range cannot.
func TestClientIPTrustedCIDRCoversContainedIPs(t *testing.T) {
	tpWithProxies(t, "10.0.0.0/8")
	if got := clientIP(tpRequest("8.8.8.8, 10.1.2.3", "203.0.113.9:5555")); got != "8.8.8.8" {
		t.Errorf("clientIP = %q, want 8.8.8.8 (10.1.2.3 is inside 10.0.0.0/8)", got)
	}
	// 11.0.0.1 is outside the CIDR, so it stands as the client even though a
	// trusted hop follows it.
	if got := clientIP(tpRequest("11.0.0.1, 10.1.2.3", "203.0.113.9:5555")); got != "11.0.0.1" {
		t.Errorf("clientIP = %q, want 11.0.0.1 (outside 10.0.0.0/8)", got)
	}
}
