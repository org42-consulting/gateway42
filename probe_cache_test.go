package main

import (
	"context"
	"testing"
	"time"
)

// A probe that is already running when the cache is invalidated read the
// engine before the change — a pull, a delete, an engine edit. Letting it
// store its result puts the old model list back into the cache that was just
// cleared, so the change stays invisible for another probeCacheTTL (and the
// routing index built from it for routeIndexTTL).

// fakeEngine is an Engine whose probe answers are scripted. ListModels blocks
// on gate, when set, so a test can hold a probe in flight.
type fakeEngine struct {
	models []string
	gate   chan struct{}
	stream ChatStream
	*concurrencyLimiter
}

func (f *fakeEngine) Type() string          { return EngineOpenAICompat }
func (f *fakeEngine) Name() string          { return "fake" }
func (f *fakeEngine) baseURL() string       { return "http://fake" }
func (f *fakeEngine) Status() (bool, error) { return true, nil }
func (f *fakeEngine) ListModels() ([]map[string]interface{}, error) {
	if f.gate != nil {
		<-f.gate
	}
	out := make([]map[string]interface{}, 0, len(f.models))
	for _, m := range f.models {
		out = append(out, map[string]interface{}{"id": m})
	}
	return out, nil
}
func (f *fakeEngine) Chat(context.Context, map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakeEngine) StreamChat(context.Context, map[string]interface{}) (ChatStream, error) {
	return f.stream, nil
}

func resetProbeCache(t *testing.T) {
	t.Helper()
	invalidateProbeCache()
	t.Cleanup(invalidateProbeCache)
}

func TestProbeStartedBeforeInvalidationIsNotCached(t *testing.T) {
	resetProbeCache(t)
	stale := &fakeEngine{models: []string{"old"}, gate: make(chan struct{})}

	done := make(chan probeData)
	go func() { done <- probeEngineCached(7, stale) }()

	// Wait until the probe has published its in-flight marker.
	deadline := time.Now().Add(2 * time.Second)
	for {
		probeCacheMu.Lock()
		_, inflight := probeCache[7]
		probeCacheMu.Unlock()
		if inflight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe never started")
		}
		time.Sleep(time.Millisecond)
	}

	invalidateProbeCache() // e.g. a pull just finished
	close(stale.gate)
	<-done

	fresh := &fakeEngine{models: []string{"new"}}
	got := probeEngineCached(7, fresh)
	if len(got.models) != 1 || got.models[0] != "new" {
		t.Errorf("models = %v after invalidation, want a fresh probe [new]; "+
			"the pre-invalidation probe repopulated the cache", got.models)
	}
}

func TestRouteIndexBuiltBeforeInvalidationIsDiscarded(t *testing.T) {
	invalidateRouteIndex()
	t.Cleanup(invalidateRouteIndex)

	gen := routeIndexGeneration()
	invalidateRouteIndex()
	storeRouteIndex(&routeIndex{byModel: map[string][]int{"old": {1}}, built: time.Now()}, gen)

	if idx := routeIndexPtr.Load(); idx != nil {
		t.Errorf("stale index stored after invalidation: %v", idx.byModel)
	}

	gen = routeIndexGeneration()
	storeRouteIndex(&routeIndex{byModel: map[string][]int{"new": {1}}, built: time.Now()}, gen)
	if idx := routeIndexPtr.Load(); idx == nil {
		t.Error("an index built after the last invalidation must be stored")
	}
}

// The sidebar reads engine state straight from the probe cache. The case that
// matters most is the first probe still in flight: its entry has a zero
// status, which must read as "unknown", not as an engine that is down.
func TestPeekProbeState(t *testing.T) {
	resetProbeCache(t)
	now := time.Now()
	running := make(chan struct{})
	probeCacheMu.Lock()
	probeCache[1] = probeEntry{inflight: running}
	probeCache[2] = probeEntry{probeData: probeData{status: true}, expiry: now.Add(time.Minute)}
	probeCache[3] = probeEntry{probeData: probeData{status: false}, expiry: now.Add(time.Minute)}
	probeCache[4] = probeEntry{probeData: probeData{status: true}, expiry: now.Add(-time.Second)}
	probeCache[5] = probeEntry{probeData: probeData{status: false}, expiry: now.Add(-time.Second), inflight: running}
	probeCacheMu.Unlock()

	tests := []struct {
		name      string
		id        int
		wantState string
		wantStale bool
	}{
		{"never probed", 99, "unknown", true},
		{"first probe in flight", 1, "unknown", false},
		{"fresh, up", 2, "online", false},
		{"fresh, down", 3, "offline", false},
		{"expired shows last result", 4, "online", true},
		{"expired, refresh running", 5, "offline", false},
	}
	for _, tt := range tests {
		state, stale := peekProbeState(tt.id)
		if state != tt.wantState || stale != tt.wantStale {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tt.name, state, stale, tt.wantState, tt.wantStale)
		}
	}
}
