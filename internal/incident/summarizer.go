// Package incident writes a short, human-readable summary when a node is
// quarantined, so the on-call engineer sees "what broke and what to do" on the
// node itself. An LLM is optional: the template summarizer needs no network.
package incident

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Incident is what the controller knows when it quarantines a node.
type Incident struct {
	Node     string
	GPUModel string
	Message  string // the agent's condition message, e.g. "GPU2: XID 79 (...)"
	Evicted  int
	Blocked  int // evictions refused by a PodDisruptionBudget
}

// Summarizer turns an Incident into a short text summary.
type Summarizer interface {
	Summarize(ctx context.Context, in Incident) (string, error)
}

// Template is a deterministic summarizer with hand-written runbook hints.
type Template struct{}

var hints = []struct{ needle, hint string }{
	{"XID 79", "GPU fell off the PCIe bus: reboot the node; if it recurs, open a hardware ticket."},
	{"XID 48", "Uncorrectable memory error: reset the GPU and run DCGM diagnostics (dcgmi diag -r 3)."},
	{"double-bit", "Uncorrectable memory error: reset the GPU and run DCGM diagnostics (dcgmi diag -r 3)."},
	{"row remapping", "Row remapping failed: GPU memory cannot self-heal; RMA the GPU."},
	{"temperature", "Overheating: check fans/cooling loop and inlet temperature before returning to service."},
	{"NVLink", "NVLink errors: run NVLink bandwidth tests and reseat or replace the bridge/baseboard."},
	{"expected GPUs", "A GPU is missing: check nvidia-smi and dmesg for PCIe errors; reboot, then RMA if persistent."},
	{"GSP", "GPU System Processor error: update the driver/firmware and reset the GPU."},
}

// Summarize renders the incident with a matching runbook hint.
func (Template) Summarize(_ context.Context, in Incident) (string, error) {
	hint := "Run DCGM diagnostics (dcgmi diag -r 3) and review the agent's /debug/report."
	for _, h := range hints {
		if strings.Contains(in.Message, h.needle) {
			hint = h.hint
			break
		}
	}
	s := fmt.Sprintf("Quarantined %s: %s. Evicted %d pod(s)", in.Node, in.Message, in.Evicted)
	if in.Blocked > 0 {
		s += fmt.Sprintf(", %d blocked by PodDisruptionBudget", in.Blocked)
	}
	return s + ". Next step: " + hint, nil
}

// ChatCompletions calls any OpenAI-compatible chat endpoint: Ollama or vLLM
// (BaseURL like http://ollama:11434/v1), or Azure OpenAI when AzureDeployment
// is set (BaseURL like https://<resource>.openai.azure.com).
type ChatCompletions struct {
	BaseURL         string
	Model           string
	APIKey          string
	AzureDeployment string
	AzureAPIVersion string
	HTTP            *http.Client
	Fallback        Summarizer // used when the LLM call fails
}

const systemPrompt = `You are an SRE assistant for a GPU cluster. Given a quarantined node's ` +
	`health findings, reply with at most 3 short sentences: what failed, likely cause, and the ` +
	`next remediation step. No speculation beyond the findings. Plain text only.`

func (c ChatCompletions) endpoint() (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("incident: BaseURL is required")
	}
	if c.AzureDeployment != "" {
		v := c.AzureAPIVersion
		if v == "" {
			v = "2024-10-21"
		}
		return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
			base, url.PathEscape(c.AzureDeployment), url.QueryEscape(v)), nil
	}
	return base + "/chat/completions", nil
}

// Summarize asks the model for a summary, using Fallback if the call fails.
func (c ChatCompletions) Summarize(ctx context.Context, in Incident) (string, error) {
	out, err := c.call(ctx, in)
	if err != nil && c.Fallback != nil {
		return c.Fallback.Summarize(ctx, in)
	}
	return out, err
}

func (c ChatCompletions) call(ctx context.Context, in Incident) (string, error) {
	ep, err := c.endpoint()
	if err != nil {
		return "", err
	}
	user := fmt.Sprintf("Node: %s\nGPU model: %s\nFindings: %s\nPods evicted: %d\nEvictions blocked by PDB: %d",
		in.Node, in.GPUModel, in.Message, in.Evicted, in.Blocked)
	body := map[string]any{
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": user},
		},
		"temperature": 0.1,
		"max_tokens":  200,
	}
	if c.AzureDeployment == "" {
		body["model"] = c.Model
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		if c.AzureDeployment != "" {
			req.Header.Set("api-key", c.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("incident: llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("incident: llm HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("incident: decode llm response: %w", err)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("incident: llm returned no content")
	}
	return truncate(strings.TrimSpace(parsed.Choices[0].Message.Content), 1000), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
