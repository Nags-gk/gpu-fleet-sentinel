package incident

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode/utf8"
)

// FuzzChatCompletionsResponse treats the LLM endpoint as hostile: whatever it
// returns, the summarizer must not panic, and a summary it accepts must be
// bounded valid UTF-8 (it is stored in a node annotation).
func FuzzChatCompletionsResponse(f *testing.F) {
	f.Add(200, `{"choices":[{"message":{"content":"Replace the GPU."}}]}`)
	f.Add(200, `{"choices":[]}`)
	f.Add(200, `{"choices":[{"message":{"content":"   "}}]}`)
	f.Add(500, `upstream exploded`)
	f.Add(200, `{"choices":[{"message":{"content":"`+string(make([]byte, 3))+`"}}]}`)
	f.Add(200, `{"choices":[{"message":{"content":"日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語"}}]}`)
	f.Fuzz(func(t *testing.T, status int, body string) {
		if status < 200 || status > 599 {
			status = 200
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()

		c := ChatCompletions{BaseURL: srv.URL, Model: "m"}
		in := Incident{Node: "n", Message: "GPU0: XID 79"}
		out, err := c.Summarize(context.Background(), in)
		if err == nil {
			if out == "" {
				t.Fatal("accepted an empty summary")
			}
			if len(out) > 1000 {
				t.Fatalf("summary is %d bytes, limit 1000", len(out))
			}
			if utf8.ValidString(body) && !utf8.ValidString(out) {
				t.Fatalf("invalid UTF-8 in summary: %q", out)
			}
		}

		// With the template fallback, a bad endpoint must never fail the summary.
		c.Fallback = Template{}
		if out, err := c.Summarize(context.Background(), in); err != nil || out == "" {
			t.Fatalf("fallback did not rescue the summary: %q, %v", out, err)
		}
	})
}
