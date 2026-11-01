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
