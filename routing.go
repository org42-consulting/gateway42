package main

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Model-aware engine routing.
//
// Before this, every completion went to the first Ollama engine (or the first
// engine of any type), and the request's "model" field was read only for the
// audit log. Multiple engines could be registered and listed but only one was
// ever reachable, so asking for a model held by the second engine produced a
// confusing "model not found" from the *first* one.
//
// Routing is probe-derived: the index is built from what each engine reports
// via ListModels, so no extra configuration is needed and a model pulled onto
// any engine becomes routable as soon as the index refreshes. When two engines
// serve the same model name the lowest engine ID wins, which keeps the choice
// deterministic rather than dependent on map iteration order.

// routeIndexTTL is deliberately longer than probeCacheTTL: the set of models an
// engine holds changes only on an explicit pull or delete, both of which call
// invalidateProbeCache and drop this index with it.
const routeIndexTTL = 60 * time.Second

type routeIndex struct {
	byModel map[string][]int // model name → engine IDs serving it, ascending
	built   time.Time
	// probed is true when at least one engine answered. An index that is
	// empty because every engine is unreachable must not be read as "that
	// model does not exist".
	probed bool
}

var (
	routeIndexPtr   atomic.Pointer[routeIndex]
	routeRefreshing atomic.Bool
)

// buildRouteIndex probes every configured engine and assembles the index.
// Callers must not hold probeCacheMu.
func buildRouteIndex(ctx context.Context) *routeIndex {
	engines := cachedEngines()
	idx := &routeIndex{byModel: make(map[string][]int), built: time.Now()}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, e := range engines {
		adapter := cachedAdapter(e.ID)
		if adapter == nil {
			continue
		}
		wg.Add(1)
		go func(id int, ad Engine) {
			defer wg.Done()
			pd := probeEngineCached(id, ad)
			if !pd.status {
				return
			}
			mu.Lock()
			idx.probed = true
			for _, name := range pd.models {
				idx.byModel[name] = append(idx.byModel[name], id)
			}
			mu.Unlock()
		}(e.ID, adapter)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// Partial index. Probes still running will land in the probe cache
		// and be picked up by the next rebuild.
	}

	mu.Lock()
	defer mu.Unlock()
	for name := range idx.byModel {
		sort.Ints(idx.byModel[name])
	}
	return idx
}

// currentRouteIndex returns the index, rebuilding synchronously only when there
// is nothing to serve. A stale index is returned immediately and refreshed in
// the background, so routing never blocks a request on a slow engine twice.
func currentRouteIndex() *routeIndex {
	idx := routeIndexPtr.Load()
	if idx == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		idx = buildRouteIndex(ctx)
		routeIndexPtr.Store(idx)
		return idx
	}
	if time.Since(idx.built) > routeIndexTTL {
		refreshRouteIndexAsync()
	}
	return idx
}

// refreshRouteIndexAsync rebuilds the index off the request path, at most one
// rebuild at a time.
func refreshRouteIndexAsync() {
	if !routeRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer routeRefreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		routeIndexPtr.Store(buildRouteIndex(ctx))
	}()
}

// invalidateRouteIndex drops the index so the next lookup rebuilds it. Called
// alongside invalidateProbeCache whenever engines or models change.
func invalidateRouteIndex() {
	routeIndexPtr.Store(nil)
}

// defaultEngine is the pre-routing behaviour, kept for requests that name no
// model and as the fallback when nothing has been successfully probed.
func defaultEngine() Engine {
	if ollamas := cachedAdaptersByType(EngineOllama); len(ollamas) > 0 {
		return ollamas[0]
	}
	if engines := cachedEngines(); len(engines) > 0 {
		return cachedAdapter(engines[0].ID)
	}
	return nil
}

// routeErr distinguishes "nothing is configured" from "that model is not on any
// engine", which map to different HTTP statuses.
type routeErr int

const (
	routeOK routeErr = iota
	routeNoEngines
	routeModelNotFound
)

// selectEngine resolves the engine that should serve a completion for model.
//
// An unknown model is a 404 only when at least one engine answered its probe;
// if every engine is unreachable the index is empty for reasons that have
// nothing to do with the model name, so we fall back to the default engine and
// let the upstream failure surface as a 502.
func selectEngine(model string) (Engine, routeErr) {
	if len(cachedEngines()) == 0 {
		return nil, routeNoEngines
	}
	if model == "" {
		if e := defaultEngine(); e != nil {
			return e, routeOK
		}
		return nil, routeNoEngines
	}

	idx := currentRouteIndex()
	if ids := idx.byModel[model]; len(ids) > 0 {
		for _, id := range ids {
			if a := cachedAdapter(id); a != nil {
				return a, routeOK
			}
		}
	}

	if !idx.probed {
		slog.Warn("routing: no engine probed successfully, using default",
			"model", model)
		if e := defaultEngine(); e != nil {
			return e, routeOK
		}
		return nil, routeNoEngines
	}
	return nil, routeModelNotFound
}
