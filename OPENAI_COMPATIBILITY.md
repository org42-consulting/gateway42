# OpenAI Compatibility in Gateway42

This document describes how Gateway42 provides an OpenAI-compatible API interface.

## Overview

Gateway42 exposes an OpenAI-compatible API in front of whatever engines you have configured —
Ollama, oMLX, LM Studio, or OpenAI itself. Applications built against the OpenAI SDKs can point at
Gateway42 unchanged and gain its authentication, per-user rate limiting, and audit logging.

## Supported Endpoints

Only these two paths exist under `/v1/`. Anything else returns `404`.

| Method | Path | Purpose |
| ------ | ---- | ------- |
| `POST` | `/v1/chat/completions` | Chat completion, streaming or not |
| `GET` | `/v1/models` | List models across configured engines |

`OPTIONS` on any `/v1/` path is answered as a CORS preflight.

### 1. Chat Completions

**POST /v1/chat/completions**

#### Request Format
```json
{
  "model": "llama3.2:latest",
  "messages": [
    {"role": "user", "content": "Hello"}
  ],
  "stream": false,
  "temperature": 0.7,
  "top_p": 0.9,
  "max_tokens": 1000,
  "frequency_penalty": 0.0,
  "presence_penalty": 0.0
}
```

#### Response Format
```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "llama3.2:latest",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 15,
    "total_tokens": 25
  }
}
```

`finish_reason` is always `"stop"` on the Ollama path — Gateway42 does not currently distinguish a
length-truncated response from a naturally completed one.

### 2. Model Listing

**GET /v1/models**

#### Response Format
```json
{
  "object": "list",
  "data": [
    {
      "id": "llama3.2:latest",
      "object": "model",
      "created": 1234567890,
      "owned_by": "ollama",
      "size": 2019393189
    }
  ]
}
```

`size` (bytes on disk) is a Gateway42 extension, not part of the OpenAI schema; SDKs ignore unknown
fields. `created` comes from the engine where available and falls back to the current time rather
than `0`, which some clients render as 1970.

## Engine Routing

Gateway42 routes a chat completion to an engine that actually holds the requested `model`.

The routing index is derived from the same probes that drive the dashboard — no per-model
configuration is needed, and adding a model to an engine makes it routable without a restart. When
several engines report the same model, the lowest engine ID wins, which makes routing deterministic
and makes "first configured engine" the tiebreak rather than the rule.

Consequences worth knowing:

- Requesting a model that no reachable engine reports returns `404`, not an upstream error. This is
  the one case where Gateway42 answers on the engine's behalf.
- An omitted or empty `model` falls back to the default engine (the first configured Ollama engine,
  or the first engine of any type if none is Ollama), matching the previous behaviour.
- If *every* engine is unreachable, the gateway does not conclude that the model is missing; it
  falls back to the default engine so the failure is reported as `502` from the engine rather than
  as a misleading `404`.
- The index is cached for 60 seconds and refreshed in the background, so a routing lookup never
  blocks on a probe. Model sets change only on a pull or delete, both of which invalidate the cache.

`/v1/models` aggregates across every reachable engine and reports `owned_by` as the type of the
engine that actually holds each model. A model present on two engines appears once. An engine that
is down contributes nothing rather than failing the whole listing — `502` is returned only when no
engine is reachable at all.

## Parameter Translation

For OpenAI-compatible engines the request is forwarded through an allow-list that includes the
tool-calling and structured-output fields (`tools`, `tool_choice`, `parallel_tool_calls`,
`response_format`, `logit_bias`, `logprobs`, `top_logprobs`, `n`, `user`, `service_tier`,
`max_completion_tokens`, `provider_options`) — whether they work depends on the upstream engine, not
on Gateway42. Anything outside that list is dropped.

For Ollama engines, parameters are instead mapped into Ollama's `options` object:

| OpenAI | Ollama | Note |
| ------ | ------ | ---- |
| `temperature` | `temperature` | Direct |
| `top_p` | `top_p` | Direct |
| `seed` | `seed` | Direct |
| `max_tokens` | `num_predict` | Direct |
| `stop` | `stop` | A bare string is wrapped into a list |
| `frequency_penalty` | `repeat_penalty` | `1.0 + value` |
| `presence_penalty` | `repeat_last_n` | Approximate: Ollama has no presence penalty, and `repeat_last_n` is a lookback window in tokens, not a penalty weight. Treat this mapping as lossy. |

On the Ollama path anything not in the table above is dropped rather than rejected — including
`tools` and `response_format`, so a tool-calling request sent to an Ollama engine silently degrades
to a plain completion.

## Authentication

All `/v1/` requests require a Bearer token:

```
Authorization: Bearer <api_key>
```

Keys are issued per user from the admin Dashboard and stored as SHA-256 hashes, so a key is
displayed exactly once, at creation. A disabled user's key stops working immediately.

## Streaming Support

- Set `"stream": true` in the request
- Responses are Server-Sent Events, each line prefixed `data: `, terminated by `data: [DONE]`
- The final chunk before `[DONE]` carries `usage`
- The first chunk's delta includes `"role": "assistant"`

## Errors

Errors use the OpenAI error envelope:

```json
{"error": {"message": "Rate limit exceeded", "type": "rate_limit_error", "code": null}}
```

| Status | `type` | Cause |
| ------ | ------ | ----- |
| `400` | `invalid_request_error` | Body is not valid JSON, or `messages` is absent |
| `401` | `authentication_error` | Missing, malformed, unknown, or disabled API key |
| `404` | `invalid_request_error` | The requested `model` is not available on any configured engine |
| `413` | `invalid_request_error` | Body exceeds 8 × `MAX_MESSAGE_LENGTH` |
| `429` | `rate_limit_error` | Per-user token bucket empty |
| `502` | `api_error` | No engines configured, or the engine was unreachable |
| `503` | `api_error` | Engine at `GW42_UPSTREAM_CONCURRENCY` in-flight requests; sent with `Retry-After: 2` |

Note that `503` is returned rather than queueing the request — the gateway sheds load instead of
absorbing it. Clients should honour `Retry-After`.

A `404` is a routing verdict, not an upstream response: it means the model appears on none of the
engines Gateway42 could reach. See [Engine Routing](#engine-routing) for how the index is built and
what happens when every engine is down.

Once a stream has started the HTTP status is already committed, so a mid-stream upstream failure
cannot be reported as one. Gateway42 emits an error-shaped SSE frame followed by `[DONE]`:

```
data: {"error": {"message": "Stream interrupted", "type": "api_error", "code": null}}

data: [DONE]
```

A client that accumulates deltas should treat a frame carrying `error` as a truncated completion
rather than as content.

## Usage Examples

### curl

```bash
# Chat completion (non-streaming)
curl http://localhost:7000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY_HERE" \
  -d '{
    "model": "llama3.2:latest",
    "messages": [{"role": "user", "content": "Explain quantum computing"}]
  }'

# Chat completion (streaming)
curl http://localhost:7000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY_HERE" \
  -d '{
    "model": "llama3.2:latest",
    "messages": [{"role": "user", "content": "Explain quantum computing"}],
    "stream": true
  }'

# List models
curl http://localhost:7000/v1/models \
  -H "Authorization: Bearer YOUR_API_KEY_HERE"
```

### Python with the OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:7000/v1",
    api_key="your_api_key_here",
)

response = client.chat.completions.create(
    model="llama3.2:latest",
    messages=[
        {"role": "user", "content": "Explain quantum computing"}
    ],
)
print(response.choices[0].message.content)
```

## Implementation Details

The `/v1` handlers live in `api_handlers.go`, kept apart from the admin web UI in `handlers.go` so a
change to the admin panel cannot alter this contract.

The translation layer lives in `openai.go`:

- `openAIToOllama` maps OpenAI parameters onto Ollama's `options` object
- `ollamaToOpenAI` wraps a complete Ollama reply in the `chat.completion` envelope
- `formatStreamChunk` converts each Ollama streaming line into a `chat.completion.chunk`
- `openaiError` builds the error envelope above

The response bodies themselves are typed structs in `types.go`. Only what Gateway42 *authors* is
typed — completions, chunks, the model list and errors. Inbound requests and the bodies relayed from
OpenAI-compatible engines stay as maps on purpose, so unrecognised parameters and unmodelled
response fields pass through instead of being dropped.

Engine-specific transport is in `engine.go`, behind the `Engine` interface implemented by
`OllamaAdapter` and `OpenAICompatAdapter`. Streaming crosses that interface as a `ChatStream`
(`Recv() ([]byte, error)`, terminated by `io.EOF`) rather than an `http.ResponseWriter`: adapters
produce OpenAI-shaped payloads, and `api_handlers.go` owns the SSE framing, flushing and `[DONE]`
for both engine types.

Model-name routing is in `routing.go`; `selectEngine` resolves a model to an adapter against the
cached routing index.

## Security Features

- Bearer-token authentication on every `/v1/` request
- Per-user token-bucket rate limiting (default 10 requests/minute)
- Per-engine concurrency caps with load shedding
- Audit logging of prompts and responses, exportable as CSV per user
- Request bodies bounded by `MAX_MESSAGE_LENGTH`
- Audit rows pruned after `GW42_LOG_RETENTION_DAYS` (default 30), so prompts and responses are not
  retained indefinitely

The `/v1` surface is token-authenticated and stateless, so it carries no CSRF protection and needs
none — there is no ambient credential a browser could attach. The admin UI is a different story: it
is cookie-authenticated, and every state-changing form there requires a CSRF token. Do not put a
`/v1` route behind the session cookie, or the two models collide.
