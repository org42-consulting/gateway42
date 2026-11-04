package main

import "testing"

// The handler decides how to read the upstream reply from data["stream"]: a
// missing value means a plain JSON body. Forwarding that absence as
// "stream": null leaves the choice to the upstream, and a strict one rejects
// null outright, so the request has to say what the handler will expect.

func TestOpenAICompatRequestStreamDefaultsToFalse(t *testing.T) {
	req := openAICompatRequest(map[string]interface{}{"model": "m"}, nil)
	if v, ok := req["stream"].(bool); !ok || v {
		t.Errorf("stream = %#v, want false when the client sent none", req["stream"])
	}
}

func TestOpenAICompatRequestKeepsExplicitStream(t *testing.T) {
	req := openAICompatRequest(map[string]interface{}{"model": "m", "stream": true}, nil)
	if req["stream"] != true {
		t.Errorf("stream = %#v, want true", req["stream"])
	}
}

func TestOpenAICompatRequestUsesSanitisedMessagesAndAllowList(t *testing.T) {
	msgs := []map[string]interface{}{{"role": "user", "content": "hi"}}
	req := openAICompatRequest(map[string]interface{}{
		"model": "m", "messages": "raw, unsanitised", "temperature": 0.2, "not_allowed": 1,
	}, msgs)

	if got, ok := req["messages"].([]map[string]interface{}); !ok || len(got) != 1 {
		t.Errorf("messages = %#v, want the sanitised slice", req["messages"])
	}
	if req["temperature"] != 0.2 {
		t.Errorf("temperature = %#v, want passed through", req["temperature"])
	}
	if _, ok := req["not_allowed"]; ok {
		t.Error("a parameter outside the allow-list was forwarded")
	}
}

// A stream carries usage only if the engine was asked for it. Without this the
// request log records zero tokens for every streamed completion to an
// OpenAI-compatible engine, which the usage panel then reads as a free request
// rather than an unmeasured one.

func TestOpenAICompatRequestAsksForUsageOnStreams(t *testing.T) {
	req := openAICompatRequest(map[string]interface{}{"model": "m", "stream": true}, nil)
	so, ok := req["stream_options"].(map[string]interface{})
	if !ok {
		t.Fatalf("stream_options = %#v, want a map on a streaming request", req["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %#v, want true", so["include_usage"])
	}
}

func TestOpenAICompatRequestCarriesClientStreamOptions(t *testing.T) {
	// include_usage wins, but the rest of the caller's object is its own
	// business and has to survive — an accurate audit is not a reason to drop
	// a feature the client asked for.
	req := openAICompatRequest(map[string]interface{}{
		"model":  "m",
		"stream": true,
		"stream_options": map[string]interface{}{
			"include_usage":          false,
			"continuous_usage_stats": true,
		},
	}, nil)
	so := req["stream_options"].(map[string]interface{})
	if so["include_usage"] != true {
		t.Errorf("include_usage = %#v, want the gateway's true over the client's false", so["include_usage"])
	}
	if so["continuous_usage_stats"] != true {
		t.Errorf("a client stream_options sub-field was dropped: %#v", so)
	}
}

func TestOpenAICompatRequestOmitsStreamOptionsWhenNotStreaming(t *testing.T) {
	// Sending stream_options to an endpoint that is about to return a plain
	// JSON body is at best ignored and, on a strict upstream, rejected.
	req := openAICompatRequest(map[string]interface{}{
		"model":          "m",
		"stream_options": map[string]interface{}{"include_usage": true},
	}, nil)
	if _, ok := req["stream_options"]; ok {
		t.Errorf("stream_options = %#v on a non-streaming request, want it absent", req["stream_options"])
	}
}

func TestOpenAICompatRequestDoesNotMutateCallerStreamOptions(t *testing.T) {
	// These builders hand back a new request; sanitize_content established the
	// same non-mutation contract for messages, and a caller that logged its own
	// request afterwards would be surprised to find it rewritten.
	client := map[string]interface{}{}
	openAICompatRequest(map[string]interface{}{"model": "m", "stream": true, "stream_options": client}, nil)
	if _, ok := client["include_usage"]; ok {
		t.Error("the gateway wrote include_usage into the caller's decoded request")
	}
}
