package main

// POST /v1/embeddings — OpenAI's embeddings API.
//
// Admission is the chat path's: API key, rate limit, daily quota, routing by
// model with failover, and a concurrency slot. OpenAI-compatible engines get
// the request on an allow-list and answer in OpenAI's shape already, so their
// body is relayed as-is; Ollama's /api/embed is translated both ways.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
)

// Embedder is implemented by engines that serve embeddings. It is separate
// from Engine so an adapter (or a test double) without embeddings support
// need not stub it; handleEmbeddings answers 400 for such an engine.
type Embedder interface {
	Embed(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error)
}

func (a *OpenAICompatAdapter) Embed(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error) {
	return postJSON(ctx, EngineOpenAICompat, "embed", a.upstream("/v1/embeddings"), a.cfg.APIKey, req)
}

func (a *OllamaAdapter) Embed(ctx context.Context, req map[string]interface{}) (map[string]interface{}, error) {
	return postJSON(ctx, EngineOllama, "embed", a.baseURL()+"/api/embed", "", req)
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

// embeddingInputIsText reports whether input is a string or a list of
// strings — the only forms Ollama accepts. OpenAI also takes token arrays.
func embeddingInputIsText(input interface{}) bool {
	switch v := input.(type) {
	case string:
		return true
	case []interface{}:
		for _, x := range v {
			if _, ok := x.(string); !ok {
				return false
			}
		}
		return len(v) > 0
	}
	return false
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

// ollamaEmbeddingRequest maps an OpenAI embeddings request onto /api/embed.
// encoding_format is not forwarded: Ollama only returns floats, and the
// response translation encodes base64 itself.
func ollamaEmbeddingRequest(data map[string]interface{}) map[string]interface{} {
	req := map[string]interface{}{"model": data["model"], "input": data["input"]}
	if d, ok := data["dimensions"]; ok {
		req["dimensions"] = d
	}
	return req
}

// ollamaToOpenAIEmbeddings translates an /api/embed response. base64 is the
// OpenAI encoding of each vector as little-endian float32s.
func ollamaToOpenAIEmbeddings(result map[string]interface{}, model string, base64Format bool) map[string]interface{} {
	vectors, _ := result["embeddings"].([]interface{})
	data := make([]interface{}, 0, len(vectors))
	for i, v := range vectors {
		floats, _ := v.([]interface{})
		var embedding interface{} = floats
		if base64Format {
			buf := make([]byte, 4*len(floats))
			for j, f := range floats {
				x, _ := f.(float64)
				binary.LittleEndian.PutUint32(buf[4*j:], math.Float32bits(float32(x)))
			}
			embedding = base64.StdEncoding.EncodeToString(buf)
		}
		data = append(data, map[string]interface{}{"object": "embedding", "index": i, "embedding": embedding})
	}
	if m, _ := result["model"].(string); m != "" {
		model = m
	}
	tokens := toInt(result["prompt_eval_count"])
	return map[string]interface{}{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage":  map[string]interface{}{"prompt_tokens": tokens, "total_tokens": tokens},
	}
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
	var req map[string]interface{}
	if adapter.Type() == EngineOllama {
		if !embeddingInputIsText(data["input"]) {
			writeOpenAIError(w, 400, "Ollama engines accept embedding input as a string or a list of strings, not tokens", "invalid_request_error")
			return
		}
		req = ollamaEmbeddingRequest(data)
	} else {
		req = openAICompatEmbeddingRequest(data)
	}

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
	if adapter.Type() == EngineOllama {
		format, _ := data["encoding_format"].(string)
		payload = ollamaToOpenAIEmbeddings(result, model, format == "base64")
	}
	// The usage block (prompt_tokens) is what the request log and the daily
	// quota count, as for completions; the vectors themselves are not logged.
	recordCompletionDetail(r, payload)
	jsonResponse(w, 200, payload)
}
