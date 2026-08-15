package models

import (
	"strings"
	"testing"
)

func TestNormalizeStreamChunk(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantForward bool
		wantContain string // substring that must be in output (if forwarded)
		wantAbsent  string // substring that must NOT be in output
	}{
		{
			name:        "first chunk role only no reasoning",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}`,
			wantForward: true,
			wantAbsent:  "reasoning_content",
		},
		{
			name:        "pure reasoning chunk dropped",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":null,"reasoning_content":"let me think..."},"finish_reason":null}]}`,
			wantForward: false,
		},
		{
			name:        "mixed reasoning+content reasoning stripped content kept",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hello","reasoning_content":"thought"},"finish_reason":null}]}`,
			wantForward: true,
			wantContain: "Hello",
			wantAbsent:  "reasoning_content",
		},
		{
			name:        "stop chunk empty content MUST be forwarded",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}`,
			wantForward: true,
			wantContain: "stop",
		},
		{
			name:        "stop chunk with reasoning_content forwarded reasoning stripped",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":null,"reasoning_content":"final thought"},"finish_reason":"stop"}]}`,
			wantForward: true,
			wantContain: "stop",
			wantAbsent:  "reasoning_content",
		},
		{
			name:        "normal content chunk no reasoning fast path",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
			wantForward: true,
			wantContain: "Hi",
			wantAbsent:  "reasoning_content",
		},
		{
			name:        "no reasoning_content fast path unchanged",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
			wantForward: true,
			wantContain: "hello",
		},
		{
			// llama.cpp stop chunk with empty delta {} — must become delta:{"content":""}
			name:        "stop chunk empty delta object normalized to content empty string",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			wantForward: true,
			wantContain: `"content":""`,
		},
		{
			// llama.cpp length chunk with empty delta {}
			name:        "length chunk empty delta object normalized",
			payload:     `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
			wantForward: true,
			wantContain: `"content":""`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, forward := NormalizeStreamChunk(tc.payload)
			if forward != tc.wantForward {
				t.Errorf("forward=%v, want %v", forward, tc.wantForward)
			}
			if tc.wantContain != "" && !strings.Contains(out, tc.wantContain) {
				t.Errorf("output %q missing expected %q", out, tc.wantContain)
			}
			if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
				t.Errorf("output %q should not contain %q", out, tc.wantAbsent)
			}
		})
	}
}
