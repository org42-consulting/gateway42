"""
Translation layer between the OpenAI Chat Completions API and Ollama's /api/chat.

All functions are pure (no I/O) so they are easy to unit-test and keep
routes.py free of format-specific logic.
"""

import secrets
import time


# ── ID generation ─────────────────────────────────────────────────────────────

def new_completion_id() -> str:
    return f"chatcmpl-{secrets.token_hex(12)}"


# ── Request translation ───────────────────────────────────────────────────────

# OpenAI parameter → Ollama option name
_OPTION_MAP: dict[str, str] = {
    "temperature":     "temperature",
    "top_p":           "top_p",
    "seed":            "seed",
    "max_tokens":      "num_predict",
    "presence_penalty": "repeat_last_n",   # closest Ollama analogue
}


def openai_to_ollama(data: dict, messages: list) -> dict:
    """Translate an OpenAI chat completions request body to Ollama /api/chat format.

    Args:
        data:     The parsed OpenAI request body.
        messages: Pre-sanitised message list (content already truncated).

    Returns:
        A dict ready to POST to Ollama's /api/chat endpoint.
    """
    options: dict = {}

    for openai_key, ollama_key in _OPTION_MAP.items():
        if openai_key in data:
            options[ollama_key] = data[openai_key]

    # frequency_penalty maps to Ollama's repeat_penalty (OpenAI default 0 → Ollama 1.0)
    if "frequency_penalty" in data:
        options["repeat_penalty"] = 1.0 + float(data["frequency_penalty"])

    # stop can be a string or list in the OpenAI spec
    if "stop" in data:
        stops = data["stop"]
        options["stop"] = stops if isinstance(stops, list) else [stops]

    ollama_req: dict = {
        "model":    data.get("model", "llama3.2:latest"),
        "messages": messages,
        "stream":   bool(data.get("stream", False)),
    }
    if options:
        ollama_req["options"] = options

    return ollama_req


# ── Non-streaming response translation ────────────────────────────────────────

def ollama_to_openai(ollama: dict) -> dict:
    """Translate a complete (non-streaming) Ollama /api/chat response to OpenAI format."""
    message = ollama.get("message", {})
    prompt_tokens     = ollama.get("prompt_eval_count", 0)
    completion_tokens = ollama.get("eval_count", 0)

    return {
        "id":      new_completion_id(),
        "object":  "chat.completion",
        "created": int(time.time()),
        "model":   ollama.get("model", ""),
        "choices": [
            {
                "index":         0,
                "message": {
                    "role":    message.get("role", "assistant"),
                    "content": message.get("content", ""),
                },
                "finish_reason": "stop",
            }
        ],
        "usage": {
            "prompt_tokens":     prompt_tokens,
            "completion_tokens": completion_tokens,
            "total_tokens":      prompt_tokens + completion_tokens,
        },
    }


# ── Streaming chunk translation ───────────────────────────────────────────────

def format_stream_chunk(chunk: dict, completion_id: str, is_first: bool) -> dict:
    """Translate one Ollama streaming JSON line to an OpenAI SSE chunk dict.

    OpenAI streaming convention:
      - First chunk: delta contains {"role": "assistant", "content": "<first token>"}
      - Middle chunks: delta contains {"content": "<token>"}
      - Final chunk (done=true): delta is {}, finish_reason is "stop", usage included
    """
    done    = chunk.get("done", False)
    content = chunk.get("message", {}).get("content", "")

    if is_first:
        delta: dict = {"role": "assistant", "content": content}
    elif done:
        delta = {}
    else:
        delta = {"content": content}

    result: dict = {
        "id":      completion_id,
        "object":  "chat.completion.chunk",
        "created": int(time.time()),
        "model":   chunk.get("model", ""),
        "choices": [
            {
                "index":         0,
                "delta":         delta,
                "finish_reason": "stop" if done else None,
            }
        ],
    }

    if done:
        prompt_tokens     = chunk.get("prompt_eval_count", 0)
        completion_tokens = chunk.get("eval_count", 0)
        result["usage"] = {
            "prompt_tokens":     prompt_tokens,
            "completion_tokens": completion_tokens,
            "total_tokens":      prompt_tokens + completion_tokens,
        }

    return result


# ── Models listing translation ────────────────────────────────────────────────

def ollama_tags_to_openai_models(tags: dict) -> dict:
    """Translate Ollama GET /api/tags to OpenAI GET /v1/models format."""
    data = []
    for m in tags.get("models", []):
        data.append({
            "id":       m["name"],
            "object":   "model",
            "created":  _parse_ollama_ts(m.get("modified_at", "")),
            "owned_by": "ollama",
        })
    return {"object": "list", "data": data}


def _parse_ollama_ts(ts: str) -> int:
    """Convert an Ollama ISO-8601 timestamp string to a Unix epoch int."""
    if not ts:
        return int(time.time())
    try:
        from datetime import datetime
        return int(datetime.fromisoformat(ts[:19]).timestamp())
    except Exception:
        return int(time.time())


# ── Error helpers ─────────────────────────────────────────────────────────────

def openai_error(
    message:    str,
    error_type: str = "invalid_request_error",
    code:       str | None = None,
) -> dict:
    """Build an OpenAI-format error response body."""
    return {"error": {"message": message, "type": error_type, "code": code}}
