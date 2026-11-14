package main

// Remote images for Ollama. Ollama's /api/chat takes images only as base64,
// so an image given by URL — OpenAI's image_url with an http(s) address, or an
// Anthropic image with a "url" source — used to be dropped on the way to it.
// The gateway now fetches such an image and inlines it, the way the OpenAI and
// Anthropic APIs fetch it themselves.
//
// Fetching a URL a client chose is a server-side request forgery risk: without
// care, a caller could make the gateway read an address only it can reach (a
// router admin page, a cloud metadata endpoint, another engine). The fetcher
// therefore refuses, at connect time, every address that is not public —
// loopback, private, link-local, CGNAT, multicast, unspecified — so a hostname
// that resolves (or re-resolves, mid-redirect) to one is refused too. It also
// bounds time and size, and accepts only image content types.
// GW42_FETCH_IMAGE_URLS=false turns fetching off; URL images are then dropped
// for Ollama as before.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxRemoteImageBytes = 20 << 20
	remoteImageTimeout  = 15 * time.Second
)

var errPrivateAddress = errors.New("refusing to fetch from a non-public address")

// publicOnlyDialer connects only to public unicast addresses. The check runs
// on the address actually dialled, after DNS, which is what defeats DNS
// rebinding.
var publicOnlyDialer = &net.Dialer{
	Timeout: 5 * time.Second,
	Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !isPublicAddr(ip) {
			return errPrivateAddress
		}
		return nil
	},
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}

var imageFetchClient = &http.Client{
	Timeout: remoteImageTimeout,
	Transport: &http.Transport{
		DialContext:           publicOnlyDialer.DialContext,
		Proxy:                 nil, // a proxy would dial on our behalf, bypassing the check
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("redirect to a non-HTTP URL")
		}
		return nil
	},
}

// imageCache keeps recently fetched images: a conversation resends its
// earlier images on every turn, and refetching each one per turn would cost
// time and could return different bytes, breaking the engine's prefix cache.
// Bounded by entry count and per-entry size.
var imageCache = struct {
	sync.Mutex
	entries map[string]cachedImage
}{entries: map[string]cachedImage{}}

type cachedImage struct {
	dataURL string
	expires time.Time
}

const (
	imageCacheEntries  = 32
	imageCacheMaxBytes = 6 << 20 // data: URL length, ~4.5 MB of image
	imageCacheTTL      = 30 * time.Minute
)

func cachedImageDataURL(ctx context.Context, rawURL string) (string, error) {
	now := time.Now()
	imageCache.Lock()
	if e, ok := imageCache.entries[rawURL]; ok && now.Before(e.expires) {
		imageCache.Unlock()
		return e.dataURL, nil
	}
	imageCache.Unlock()

	dataURL, err := fetchImageDataURL(ctx, rawURL)
	if err != nil || len(dataURL) > imageCacheMaxBytes {
		return dataURL, err
	}
	imageCache.Lock()
	defer imageCache.Unlock()
	if len(imageCache.entries) >= imageCacheEntries {
		var oldest string
		for k, e := range imageCache.entries {
			if oldest == "" || e.expires.Before(imageCache.entries[oldest].expires) {
				oldest = k
			}
		}
		delete(imageCache.entries, oldest)
	}
	imageCache.entries[rawURL] = cachedImage{dataURL: dataURL, expires: now.Add(imageCacheTTL)}
	return dataURL, nil
}

// fetchImageDataURL downloads an http(s) image and returns it as a data: URL.
func fetchImageDataURL(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "image/*")
	resp, err := imageFetchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image fetch returned %d", resp.StatusCode)
	}
	mt := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !strings.HasPrefix(mt, "image/") {
		return "", fmt.Errorf("not an image: %q", mt)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteImageBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxRemoteImageBytes {
		return "", errors.New("image larger than 20 MB")
	}
	return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// inlineRemoteImages returns messages with every http(s) image_url replaced
// by the fetched image as a data: URL. A part that cannot be fetched becomes a
// short text note, so the model learns an image was meant to be there rather
// than silently answering without it. Messages without remote images are
// returned as they are; changed ones are copies.
func inlineRemoteImages(ctx context.Context, messages []map[string]interface{}) []map[string]interface{} {
	if !cfg.FetchImageURLs {
		return messages
	}
	out := messages
	copied := false
	for i, m := range messages {
		parts, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		var newParts []interface{}
		for j, p := range parts {
			part, _ := p.(map[string]interface{})
			u := remoteImageURL(part)
			if u == "" {
				continue
			}
			if newParts == nil {
				newParts = append([]interface{}(nil), parts...)
			}
			if dataURL, err := cachedImageDataURL(ctx, u); err == nil {
				newParts[j] = map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}}
			} else {
				slog.Info("remote image not inlined", "url", u, "err", err)
				newParts[j] = map[string]interface{}{"type": "text", "text": fmt.Sprintf("[Image at %s could not be loaded: %v]", u, err)}
			}
		}
		if newParts == nil {
			continue
		}
		if !copied {
			out = append([]map[string]interface{}(nil), messages...)
			copied = true
		}
		cp := make(map[string]interface{}, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["content"] = newParts
		out[i] = cp
	}
	return out
}

func remoteImageURL(part map[string]interface{}) string {
	if part["type"] != "image_url" {
		return ""
	}
	iu, _ := part["image_url"].(map[string]interface{})
	u, _ := iu["url"].(string)
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return ""
}
