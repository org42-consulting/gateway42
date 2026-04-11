# OpenAI Compatibility in Gateway42.

This document describes how Gateway42 provides an OpenAI-compatible API interface.

## Overview

Gateway42 provides a complete OpenAI-compatible API layer that allows applications to interact with local Ollama models using standard OpenAI SDKs while maintaining all the security, governance, and audit features of Gateway42.

## Supported Endpoints

### 1. Chat Completions
**POST /v1/chat/completions**

This endpoint supports both streaming and non-streaming requests.

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

### 2. Model Listing
**GET /v1/models**

Lists all available Ollama models in OpenAI format.

#### Response Format
```json
{
  "object": "list",
  "data": [
    {
      "id": "llama3.2:latest",
      "object": "model",
      "created": 1234567890,
      "owned_by": "ollama"
    }
  ]
}
```

## Authentication

All API requests require authentication via Bearer token:

```
Authorization: Bearer <api_key>
```

## Streaming Support

Gateway42 supports OpenAI streaming responses:
- Set `"stream": true` in the request
- Responses are sent as Server-Sent Events (SSE) with `data: ` prefix
- Final chunk includes usage information

## Usage Examples

### Using curl with OpenAI-compatible API

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

### Python with OpenAI SDK
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:7000/v1",
    api_key="your_api_key_here"
)

response = client.chat.completions.create(
    model="llama3.2:latest",
    messages=[
        {"role": "user", "content": "Explain quantum computing"}
    ]
)
print(response.choices[0].message.content)
```

### Legacy /chat endpoint (if available)
Gateway42 also supports a legacy `/chat` endpoint for backward compatibility:

```bash
curl http://localhost:7000/chat \
  -H "Content-Type: application/json" \
  -d '{
    "auth":{"username":"user@company.com","password":"password"},
    "messages":[{"role":"user","content":"Explain MQTT"}]
  }'
```

## Implementation Details

Gateway42 implements a translation layer in `openai_compat.py` that:
- Maps OpenAI parameters to Ollama parameters (temperature, top_p, max_tokens, etc.)
- Converts Ollama streaming responses to OpenAI-compatible SSE format
- Translates model listings from Ollama format to OpenAI format
- Handles authentication and error responses

## Security Features

- Authentication required for all API access
- Rate limiting (default 10 requests/minute)
- Audit logging of all interactions
- User isolation and access control
- Session management

Gateway42 is production-ready and allows any application using OpenAI SDKs to seamlessly connect to local Ollama models while maintaining all security features including rate limiting, audit logging, and user management.