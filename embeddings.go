package main

// POST /v1/embeddings — OpenAI's embeddings API.
//
// Admission is the chat path's: API key, rate limit, daily quota, routing by
// model with failover, and a concurrency slot. The engine (oMLX, or any
// OpenAI-compatible server) gets the request on an allow-list and answers in
// OpenAI's shape, which is relayed as-is.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// Embedder is implemented by engines that serve embeddings. It is separate
// from Engine so an adapter (or a test double) without embeddings support
// need not stub it; handleEmbeddings answers 400 for such an engine.
type Embedder interface {
	Embed(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error)
}

func (a *OpenAICompatAdapter) Embed(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error) {
	return postJSON(ctx, a.Type(), "embed", a.upstream("/v1/embeddings"), a.cfg.APIKey, req)
}

// postJSON sends one bounded JSON request to an engine and decodes the JSON
// answer; a non-200 is an upstreamStatusError, as for completions.
func postJSON(ctx context.Context, engineType, op, url, apiKey string, req map[string]interface{}) (map[string]interface{}, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamChatTimeout())
	defer cancel()

	var result map[string]interface{}
	err = observeUpstream(engineType, op, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := completionClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return newUpstreamStatusError(resp)
		}
		return json.NewDecoder(resp.Body).Decode(&result)
	})
	return result, err
}

// openAICompatEmbeddingRequest forwards the OpenAI embeddings parameters.
func openAICompatEmbeddingRequest(data map[string]interface{}) map[string]interface{} {
	req := map[string]interface{}{}
	for _, key := range []string{"model", "input", "encoding_format", "dimensions", "user"} {
		if v, ok := data[key]; ok {
			req[key] = v
		}
	}
	return req
}

func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		writeOpenAIError(w, 401, "Invalid API key", "authentication_error")
		return
	}
	if q := checkQuota(user.Name); q.quotaExceeded() {
		writeOpenAIError(w, 429, "Daily token quota exceeded", "rate_limit_error")
		return
	}
	data, ok := decodeAPIBody(w, r, writeOpenAIError)
	if !ok {
		return
	}
	model, _ := data["model"].(string)
	if model == "" {
		writeOpenAIError(w, 400, "'model' is required", "invalid_request_error")
		return
	}
	if data["input"] == nil {
		writeOpenAIError(w, 400, "'input' is required", "invalid_request_error")
		return
	}

	adapter, release, ok := acquireEngine(w, r, model, writeOpenAIError)
	if !ok {
		return
	}
	defer release()

	embedder, ok := adapter.(Embedder)
	if !ok {
		writeOpenAIError(w, 400, "The engine serving this model does not support embeddings", "invalid_request_error")
		return
	}
	req := openAICompatEmbeddingRequest(data)

	result, err := embedder.Embed(r.Context(), req)
	if err != nil {
		if r.Context().Err() != nil {
			slog.Info("client disconnected before embeddings", "model", model, "user", user.ID)
			return
		}
		slog.Error("engine embeddings", "err", err, "model", model)
		detailFromContext(r).setUpstreamErr(err)
		writeOpenAIEngineFailure(w, err)
		return
	}
	var payload interface{} = result
	// The usage block (prompt_tokens) is what the request log and the daily
	// quota count, as for completions; the vectors themselves are not logged.
	recordCompletionDetail(r, payload)
	jsonResponse(w, 200, payload)
}
