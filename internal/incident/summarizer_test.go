package incident

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var in = Incident{Node: "gpu-7", GPUModel: "H100", Message: "GPU2: XID 79 (GPU has fallen off the bus)", Evicted: 3, Blocked: 1}

func TestTemplate(t *testing.T) {
	s, err := Template{}.Summarize(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gpu-7", "Evicted 3", "1 blocked", "PCIe bus"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q: %s", want, s)
		}
	}
}

func TestOpenAICompatible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "llama3.2" {
			t.Errorf("model = %v", body["model"])
		}
		msgs := body["messages"].([]any)
		if !strings.Contains(msgs[1].(map[string]any)["content"].(string), "XID 79") {
			t.Error("findings not sent to model")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  GPU2 dropped off PCIe. Reboot node.  "}}]}`))
	}))
	defer srv.Close()

	c := ChatCompletions{BaseURL: srv.URL + "/v1", Model: "llama3.2", APIKey: "k"}
	s, err := c.Summarize(context.Background(), in)
	if err != nil || s != "GPU2 dropped off PCIe. Reboot node." {
		t.Fatalf("got %q, %v", s, err)
	}
}

func TestAzureRouting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/deployments/gpt-4o-mini/chat/completions" || r.URL.Query().Get("api-version") == "" {
			t.Errorf("bad azure url %s", r.URL.String())
		}
		if r.Header.Get("api-key") != "az" || r.Header.Get("Authorization") != "" {
			t.Error("azure must use api-key header")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := ChatCompletions{BaseURL: srv.URL, AzureDeployment: "gpt-4o-mini", APIKey: "az"}
	if s, err := c.Summarize(context.Background(), in); err != nil || s != "ok" {
		t.Fatalf("got %q %v", s, err)
	}
}

func TestFallbackOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	noFallback := ChatCompletions{BaseURL: srv.URL}
	if _, err := noFallback.Summarize(context.Background(), in); err == nil {
		t.Fatal("expected error without fallback")
	}
	withFallback := ChatCompletions{BaseURL: srv.URL, Fallback: Template{}}
	s, err := withFallback.Summarize(context.Background(), in)
	if err != nil || !strings.Contains(s, "Quarantined gpu-7") {
		t.Fatalf("fallback not used: %q %v", s, err)
	}
}
