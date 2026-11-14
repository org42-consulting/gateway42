package main

// Ollama context windows.
//
// Ollama's model listing (/api/tags) says nothing about context length, and an
// Ollama request without options.num_ctx runs with the server's default
// (OLLAMA_CONTEXT_LENGTH, or the model's Modelfile PARAMETER num_ctx) — often
// far below what the model supports, and Ollama trims an overlong prompt from
// the front without an error. Two things follow from /api/show, which reports
// both the model's native maximum (model_info "<arch>.context_length") and
// any Modelfile num_ctx:
//
//   - /v1/models can report each Ollama model's effective window as
//     max_input_tokens, so Claude Desktop sizes its context correctly;
//   - with GW42_OLLAMA_NUM_CTX set, every request carries that num_ctx,
//     clamped to the model's maximum, so the window is the operator's choice
//     instead of a silent default. It is one fixed value on purpose: Ollama
//     reloads a model whenever num_ctx changes.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ollamaModelInfo struct {
	native       int // the model's maximum context, 0 if unknown
	modelfileCtx int // PARAMETER num_ctx from the Modelfile, 0 if none
	fetched      time.Time
}

// effectiveContext is the window Ollama will use for this model: the
// gateway's num_ctx clamped to the model's maximum when one is set, else the
// Modelfile's; 0 when it depends on the server default the gateway cannot see.
func (i ollamaModelInfo) effectiveContext() int {
	if n := cfg.OllamaNumCtx; n > 0 {
		if i.native > 0 && i.native < n {
			return i.native
		}
		return n
	}
	return i.modelfileCtx
}

var ollamaInfo = struct {
	sync.Mutex
	m map[string]ollamaModelInfo
}{m: map[string]ollamaModelInfo{}}

const ollamaInfoTTL = 10 * time.Minute

// modelInfo returns /api/show facts for model, from cache when fresh. ok is
// false when the engine could not be asked within ctx.
func (a *OllamaAdapter) modelInfo(ctx context.Context, model string) (ollamaModelInfo, bool) {
	key := a.baseURL() + "\x00" + model
	ollamaInfo.Lock()
	info, cached := ollamaInfo.m[key]
	ollamaInfo.Unlock()
	if cached && time.Since(info.fetched) < ollamaInfoTTL {
		return info, true
	}

	body, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL()+"/api/show", bytes.NewReader(body))
	if err != nil {
		return info, cached
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := controlClient.Do(req)
	if err != nil {
		return info, cached
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, cached
	}
	var show struct {
		ModelInfo  map[string]interface{} `json:"model_info"`
		Parameters string                 `json:"parameters"`
	}
	if json.NewDecoder(resp.Body).Decode(&show) != nil {
		return info, cached
	}
	info = parseOllamaShow(show.ModelInfo, show.Parameters)
	info.fetched = time.Now()
	ollamaInfo.Lock()
	ollamaInfo.m[key] = info
	ollamaInfo.Unlock()
	return info, true
}

func parseOllamaShow(modelInfo map[string]interface{}, parameters string) ollamaModelInfo {
	var info ollamaModelInfo
	for k, v := range modelInfo {
		if strings.HasSuffix(k, ".context_length") {
			info.native = toInt(v)
		}
	}
	// parameters is the Modelfile's PARAMETER block, one "name value" per line.
	for _, line := range strings.Split(parameters, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "num_ctx" {
			info.modelfileCtx, _ = strconv.Atoi(f[1])
		}
	}
	return info
}

// ollamaContextWindows fetches the effective window of each named model on
// an Ollama engine, concurrently and within one deadline; models that could
// not be asked in time are absent. Cached after the first call, so only a
// cold /v1/models pays for it.
func ollamaContextWindows(a *OllamaAdapter, models []string, within time.Duration) map[string]int {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := make(map[string]int, len(models))
	for _, m := range models {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			if info, ok := a.modelInfo(ctx, m); ok {
				if n := info.effectiveContext(); n > 0 {
					mu.Lock()
					out[m] = n
					mu.Unlock()
				}
			}
		}(m)
	}
	wg.Wait()
	return out
}

// applyOllamaNumCtx sets options.num_ctx on an Ollama request when
// GW42_OLLAMA_NUM_CTX is configured, clamped to the model's maximum.
func applyOllamaNumCtx(a *OllamaAdapter, req map[string]interface{}) {
	if cfg.OllamaNumCtx <= 0 {
		return
	}
	model, _ := req["model"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	info, _ := a.modelInfo(ctx, model)
	cancel()
	n := ollamaModelInfo{native: info.native}.effectiveContext()
	options, _ := req["options"].(map[string]interface{})
	if options == nil {
		options = map[string]interface{}{}
		req["options"] = options
	}
	options["num_ctx"] = n
}
