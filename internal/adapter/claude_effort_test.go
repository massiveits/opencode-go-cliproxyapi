package adapters

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"opencode-go-cliproxyapi/internal/adapter/chatcompletions"
	"opencode-go-cliproxyapi/internal/adapter/responses"
	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
)

type claudeTarget struct {
	name  string
	build func(string, string, []byte, *pluginapi.ThinkingSupport, ...*shared.ResponseTools) ([]byte, *errclass.Error)
}

var claudeTargets = []claudeTarget{
	{"chat", chatcompletions.BuildRequest},
	{"responses", responses.BuildRequest},
}

// forwardedEffort returns the effort a target request carries, or "" when
// the field is absent.
func forwardedEffort(t *testing.T, target string, out []byte) string {
	t.Helper()
	var body struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("decode %s request: %v: %s", target, err, out)
	}
	if target == "responses" {
		if body.Reasoning == nil {
			return ""
		}
		return body.Reasoning.Effort
	}
	return body.ReasoningEffort
}

func TestClaudeExplicitEffortAcrossTargets(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{Levels: []string{"low", "high"}}
	controls := map[string]string{
		"no thinking": "",
		"adaptive":    `"thinking":{"type":"adaptive"},`,
		// Budget 1024 alone maps to "low"; explicit effort must win.
		"budget": `"thinking":{"type":"enabled","budget_tokens":1024},`,
	}
	for _, target := range claudeTargets {
		for controlName, control := range controls {
			// "max" is undeclared: there is no local validation, so it
			// forwards like any other client-declared level.
			for _, effort := range []string{"high", "max", " HIGH "} {
				for _, stream := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/%q/stream=%v", target.name, controlName, effort, stream)
					t.Run(name, func(t *testing.T) {
						body := fmt.Sprintf(`{"model":"lab","max_tokens":123,"stream":%v,%s"output_config":{"effort":%q},"messages":[{"role":"user","content":"hello"}]}`, stream, control, effort)
						out, err := target.build("lab", "claude", []byte(body), ts)
						if err != nil {
							t.Fatal(err)
						}
						want := map[string]string{"high": "high", "max": "max", " HIGH ": "high"}[effort]
						if got := forwardedEffort(t, target.name, out); got != want {
							t.Fatalf("explicit effort lost or rewritten: got %q, want %q: %s", got, want, out)
						}
					})
				}
			}
		}
	}
}

func TestClaudeEffortFallbacks(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{Levels: []string{"low", "high"}}
	for _, test := range []struct {
		name, fields       string
		wantChat, wantResp string
	}{
		{"missing effort keeps budget mapping", `"thinking":{"type":"enabled","budget_tokens":1024},`, "low", "low"},
		{"blank effort keeps budget mapping", `"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"  "},`, "low", "low"},
		{"no controls sends no effort", ``, "", ""},
		{"empty output_config sends no effort", `"output_config":{},`, "", ""},
		{"auto omitted on Responses wire", `"output_config":{"effort":"auto"},`, "auto", ""},
		{"none forwards as-is", `"output_config":{"effort":"none"},`, "none", "none"},
	} {
		for _, target := range claudeTargets {
			t.Run(target.name+"/"+test.name, func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"lab","max_tokens":123,%s"messages":[{"role":"user","content":"hello"}]}`, test.fields)
				out, err := target.build("lab", "claude", []byte(body), ts)
				if err != nil {
					t.Fatal(err)
				}
				want := test.wantChat
				if target.name == "responses" {
					want = test.wantResp
				}
				if got := forwardedEffort(t, target.name, out); got != want {
					t.Fatalf("got effort %q, want %q: %s", got, want, out)
				}
			})
		}
	}
}
