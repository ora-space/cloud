package modelgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

// This opt-in test observes the fixed native CLI against the production proxy with synthetic
// credentials and a controlled supplier. It reads no user profile or real model credential.
func TestPinnedClaudeMessagesCompatibility(t *testing.T) {
	binary := os.Getenv("CLAUDE_COMPAT_BINARY")
	if binary == "" {
		t.Skip("set CLAUDE_COMPAT_BINARY to the verified Claude 2.1.303 binary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil || !strings.Contains(string(version), "2.1.303") {
		t.Fatal("pinned CLI version unavailable")
	}
	var calls atomic.Int64
	var observedBeta atomic.Value
	service, _ := anthropicFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RawQuery != "beta=true" || r.Header.Get("Anthropic-Beta") != pinnedClaudeBetas {
			t.Error("fixed CLI protocol no longer matches the supported matrix")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			"{\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}",
			"{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
			"{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Fixture greeting.\"}}",
			"{\"type\":\"content_block_stop\",\"index\":0}",
			"{\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}",
			"{\"type\":\"message_stop\"}",
		} {
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(event), &envelope); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, "event: "+envelope.Type+"\ndata: "+event+"\n\n")
		}
	}))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			observedBeta.Store(r.Header.Get("Anthropic-Beta"))
		}
		service.ModelHandler(w, r)
	}))
	defer proxy.Close()
	root := t.TempDir()
	env := map[string]string{
		"HOME": root, "CLAUDE_CONFIG_DIR": filepath.Join(root, "config"),
		"ANTHROPIC_BASE_URL": proxy.URL + "/runtime/anthropic", "ANTHROPIC_AUTH_TOKEN": strings.Repeat("r", 40),
		"ANTHROPIC_MODEL": "fixture-model", "ANTHROPIC_DEFAULT_OPUS_MODEL": "fixture-model",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "fixture-model", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "fixture-model",
		"CLAUDE_CODE_SUBAGENT_MODEL": "fixture-model", "DISABLE_NON_ESSENTIAL_TRAFFIC": "1",
	}
	if err := os.MkdirAll(env["CLAUDE_CONFIG_DIR"], 0o700); err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(core.Object{"apiKeyHelper": "", "env": env, "disableAllHooks": true})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "-p", "Return a greeting.", "--model", "fixture-model",
		"--setting-sources", "", "--no-session-persistence", "--output-format", "json", "--max-turns", "1",
		"--settings", string(settings))
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("pinned CLI failed: %v (stdout bytes=%d upstream calls=%d beta=%v)", err, len(output), calls.Load(), observedBeta.Load())
	}
	var result core.Object
	if err := json.Unmarshal(output, &result); err != nil || result.B("is_error") || result.S("subtype") != "success" || calls.Load() != 1 {
		t.Fatalf("pinned CLI did not complete one authorized request: calls=%d", calls.Load())
	}
}
