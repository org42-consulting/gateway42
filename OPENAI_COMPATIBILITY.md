# OpenAI Compatibility in Gateway42

This document describes how Gateway42 provides an OpenAI-compatible API interface.

## Overview

Gateway42 exposes an OpenAI-compatible API in front of whatever engines you have configured —
oMLX (the first-class engine), vLLM, LM Studio, llama.cpp, or OpenAI itself. Applications built against the OpenAI SDKs can point at
Gateway42 unchanged and gain its authentication, per-user rate limiting, and audit logging.

## Supported Endpoints

Only these two paths exist under `/v1/`. Anything else returns `404`.

| Method | Path | Purpose |
| ------ | ---- | ------- |
| `POST` | `/v1/chat/completions` | Chat completion, streaming or not |
| `POST` | `/v1/embeddings` | Embeddings, relayed to the engine |
| `POST` | `/v1/responses` | Responses API, passed through to the engine (streaming or not) |
| `POST` | `/v1/completions` | Legacy completions, passed through; streams get `include_usage` |
| `POST` | `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/audio/translations` | Passed through (multipart uploads byte for byte; audio returned as bytes) |
| `POST` | `/v1/images/generations`, `/v1/moderations` | Passed through |
| `GET` | `/v1/mcp/tools`, `/v1/mcp/servers` | Backend MCP tools and servers (oMLX), combined across engines |
| `POST` | `/v1/mcp/execute` | Runs a backend MCP tool on the engine that has it |
| `GET` | `/v1/models` | List models across configured engines, with `max_input_tokens` when the engine reports a context window |

`OPTIONS` on any `/v1/` path is answered as a CORS preflight.

### 1. Chat Completions

**POST /v1/chat/completions**

#### Request Format
```json
{
  "model": "Qwen3-8B-4bit",
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
  "model": "Qwen3-8B-4bit",
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

### 2. Model Listing

**GET /v1/models**

#### Response Format
```json
{
  "object": "list",
  "data": [
    {
      "id": "Qwen3-8B-4bit",
      "object": "model",
      "created": 1234567890,
      "owned_by": "omlx",
      "max_input_tokens": 40960
    }
  ]
}
```

`size` (bytes on disk) is a Gateway42 extension, not part of the OpenAI schema; SDKs ignore unknown
fields. `created` comes from the engine where available and falls back to the current time rather
than `0`, which some clients render as 1970.

## Backend MCP tools

When an engine exposes MCP tools to its models (oMLX with an MCP configuration), chat completions
that call them are completed by the gateway: it runs the call through the engine's
`/v1/mcp/execute`, appends the result and asks the engine again, until the model answers or calls
one of the client's own tools (`GW42_MCP_MAX_ROUNDS`, default 8). The client receives one
completion: no tool call for a backend tool, the text of every round, usage summed. A stream keeps
one `id`, one role chunk and one `finish_reason`, the client's tool calls are numbered from 0, and
SSE comments keep the connection open while a tool runs. Tools the client offers itself are never
taken over, even on a name clash. `GW42_MCP=off` disables this and the `/v1/mcp/*` endpoints. See
the README's *Backend MCP tools* for details.

## Pass-through endpoints

`/v1/responses`, `/v1/completions`, `/v1/audio/*`, `/v1/images/generations` and `/v1/moderations` are
relayed to the engine's own implementation rather than translated: oMLX, vLLM and LM Studio
implement most of them, and a translation onto chat
completions would lose what makes each one different. The request is routed by its `model` (a
multipart field for audio uploads; optional for moderations, which then use the default engine)
with the usual admission and failover. Server-sent events are relayed line for line, flushed at each
event, with the stream bounded by silence; other responses are relayed with their content type.
Usage found in the response (`usage`, or a Responses API `response.completed` event) is recorded
for the request log and quota, and a streaming `/v1/completions` request is given
`stream_options.include_usage` so it can be. An engine without the endpoint answers `404`, which the
client receives with the engine's reason.

## Embeddings

`POST /v1/embeddings` takes OpenAI's body: `model`, `input` (a string, a list of strings, or token
arrays), `dimensions`, `encoding_format` and `user`. The engine receives exactly those fields and
its response is relayed unchanged. Rate limit, quota, routing with failover and engine-error handling are the same as
for chat completions.

## Engine Routing

Gateway42 routes a chat completion to an engine that actually holds the requested `model`.

The routing index is derived from the same probes that drive the dashboard — no per-model
configuration is needed, and adding a model to an engine makes it routable without a restart. When
several engines report the same model, the lowest engine ID wins, which makes routing deterministic
and makes "first configured engine" the tiebreak rather than the rule.

Consequences worth knowing:

- Requesting a model that no reachable engine reports returns `404`, not an upstream error. This is
  the one case where Gateway42 answers on the engine's behalf.
- An omitted or empty `model` falls back to the default engine: the first configured oMLX engine,
  or the first engine of any type if there is none.
- If *every* engine is unreachable, the gateway does not conclude that the model is missing; it
  falls back to the default engine so the failure is reported as `502` from the engine rather than
  as a misleading `404`.
- The index is cached for 60 seconds and refreshed in the background, so a routing lookup never
  blocks on a probe. A download, delete, load or unload from the admin page invalidates it.

`/v1/models` aggregates across every reachable engine and reports `owned_by` as the type of the
engine that actually holds each model. A model present on two engines appears once. An engine that
is down contributes nothing rather than failing the whole listing — `502` is returned only when no
engine is reachable at all.

## Parameter Translation

For OpenAI-compatible engines the request is forwarded through an allow-list that includes the
tool-calling and structured-output fields (`tools`, `tool_choice`, `parallel_tool_calls`,
`response_format`, `logit_bias`, `logprobs`, `top_logprobs`, `n`, `user`, `service_tier`,
`max_completion_tokens`, `provider_options`) and the reasoning controls (`reasoning_effort`, and
`chat_template_kwargs`, which vLLM, oMLX and LM Studio read, for example Qwen3's `enable_thinking`),
OpenAI's other chat parameters (`verbosity`, `metadata`, `store`, `modalities`, `audio`, `prediction`,
`web_search_options`, and the legacy `functions` / `function_call`), and the sampler extensions local
servers accept (`top_k`, `min_p`, `repetition_penalty`, `thinking_budget`) — whether they work depends
on the upstream engine, not on Gateway42. The local-only ones (`chat_template_kwargs`, `top_k`,
`min_p`, `repetition_penalty`, `thinking_budget`) are withheld from `api.openai.com`, which rejects
unknown parameters. Anything outside that list is dropped. `stream` is always forwarded as a boolean, `false`
when the client omitted it, never `null`.

`stream_options` is the one field the gateway rewrites rather than forwards. On a stream it is added
if absent, and `include_usage` is forced on if the client left it out or set it `false` — an engine
asked for usage is an engine that reports it, and a completion the gateway cannot measure is an audit
hole rather than a free request. Your own sub-fields ride along untouched. On a non-stream it is
dropped, because the field describes a stream that is not happening.

On both API paths, messages keep `tool_calls`, `tool_call_id` and `name`, and array content is
preserved, with each text part truncated to `GW42_MAX_INPUT_LENGTH` when that is set. For oMLX
engines, which take images only as `data:` URIs, `http(s)` image URLs are fetched by the gateway and
inlined (see `GW42_FETCH_IMAGE_URLS`: public addresses only, 15 s, 20 MB, image types only, cached
for 30 minutes because a conversation resends its images every turn); one that cannot be fetched
becomes a text note.

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

That usage chunk is guaranteed rather than best-effort: the gateway asks the engine for it with
`include_usage`, so it arrives even if your request set `include_usage: false`. It takes OpenAI's own
shape — a separate final chunk whose `choices` array is empty — because streams are forwarded
verbatim. Well-behaved SDKs skip an
empty `choices`, but strict client code that assumes every chunk carries a choice should index
defensively.

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
| `413` | `invalid_request_error` | Body exceeds `GW42_MAX_REQUEST_BYTES` (default 32 MiB) |
| `429` | `rate_limit_error` | Per-user token bucket empty |
| `400` / `404` / `413` / `429` | as the engine said | The engine rejected the request (`422` is reported as `400`); the message is the engine's own, for example a context-window overflow |
| `502` | `api_error` | No engines configured, the engine was unreachable or failed (5xx), or it rejected the gateway's engine credentials (401/403) |
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

A stream is bounded by silence, not by how long it takes: it is closed after 60 s in which the engine
sent nothing, or after 30 minutes in total. A completion that keeps producing tokens is not cut for
running long, so a long agent turn on a reasoning model arrives complete rather than truncated. A
non-streaming completion is different — it sends nothing until it is finished, so it keeps a single
total budget, `GW42_UPSTREAM_TIMEOUT` (600 s by default), without the 30 s response-header limit that
applies to streams. Streaming is still the better default for anything that may generate at length:
the client sees progress, and a stalled engine is detected after 60 s of silence instead of at the
end of the budget.

## Usage Examples

### curl

```bash
# Chat completion (non-streaming)
curl http://localhost:7000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY_HERE" \
  -d '{
    "model": "Qwen3-8B-4bit",
    "messages": [{"role": "user", "content": "Explain quantum computing"}]
  }'

# Chat completion (streaming)
curl http://localhost:7000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY_HERE" \
  -d '{
    "model": "Qwen3-8B-4bit",
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
    model="Qwen3-8B-4bit",
    messages=[
        {"role": "user", "content": "Explain quantum computing"}
    ],
)
print(response.choices[0].message.content)
```

## Implementation Details

The `/v1` handlers live in `api_handlers.go`, kept apart from the admin web UI in `handlers.go` so a
change to the admin panel cannot alter this contract.

Every engine speaks OpenAI's wire format, so there is no translation layer on this path:
`openAICompatRequest` builds the engine request from the allow-list, and `openaiError` builds the
error envelope above. The response bodies the gateway authors are typed structs in
`internal/wire` — the model list, errors, and the Anthropic translation's shapes. Inbound requests and the bodies relayed from
OpenAI-compatible engines stay as maps on purpose, so unrecognised parameters and unmodelled
response fields pass through instead of being dropped.

Engine transport is in `engine.go`, behind the `Engine` interface implemented by
`OpenAICompatAdapter` for both engine types (oMLX and OpenAI-compatible). Streaming crosses that
interface as a `ChatStream` (`Recv() ([]byte, chunkMeta, error)`, terminated by `io.EOF`) rather
than an `http.ResponseWriter`, and `api_handlers.go` owns the SSE framing, flushing and `[DONE]`.
oMLX's model management lives in `omlx.go`.

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
