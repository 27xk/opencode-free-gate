package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestOpenCodeRoutesAndHeaders(t *testing.T) {
	project := currentProject()
	for _, raw := range []string{
		"/openai/v1/chat/completions",
		"/anthropic/v1/messages",
		"/codex/v1/responses",
	} {
		if _, ok := normalizePath(project, raw); !ok {
			t.Fatalf("expected route %s to be accepted", raw)
		}
	}
	if _, ok := normalizePath(project, "/v1/chat/completions"); ok {
		t.Fatal("raw /v1 route must not bypass the OpenCode prefixes")
	}

	application := &app{gateway: newGateway(config{project: project})}
	headers := application.collectHeaders(http.Header{
		"Authorization":     []string{"Bearer client-secret"},
		"X-Opencode-Client": []string{"desktop"},
	})
	if got := headers.Get("Authorization"); got != "Bearer public" {
		t.Fatalf("upstream authorization must always be forced to Bearer public: %q", got)
	}
	if got := headers.Get("X-Opencode-Client"); got != "cli" {
		t.Fatalf("client header must be normalized to cli: %q", got)
	}
	if got := headers.Get("User-Agent"); got != opencodeUserAgent() {
		t.Fatalf("upstream requests must use the opencode user agent: %q", got)
	}
	if !project.directFallback {
		t.Fatal("OpenCode must try one direct request after proxy retries are exhausted")
	}
}

func TestModelRewriteAndAliases(t *testing.T) {
	project := currentProject()
	gw := newGateway(config{project: project})
	rename := map[string]string{
		"muse-spark-1.3-contributor-free": "muse-spark-1.3-contributor",
		"deepseek-v4-flash-free":          "deepseek-v4-flash",
		"big-pickle":                      "big-pickle",
	}
	gw.modelCache = &cachedModels{
		rename:   rename,
		redirect: buildRedirect(rename),
		loadedAt: time.Now(),
	}

	testCases := []struct {
		input    string
		expected string
	}{
		// 1. 展示名称（无 -free），请求时自动加上 -free 还原为上游模型名
		{"deepseek-v4-flash", "deepseek-v4-flash-free"},
		{"muse-spark-1.3-contributor", "muse-spark-1.3-contributor-free"},
		// 2. 客户端直接传入带 -free 的真实上游名称，保持原样
		{"deepseek-v4-flash-free", "deepseek-v4-flash-free"},
		{"muse-spark-1.3-contributor-free", "muse-spark-1.3-contributor-free"},
		// 3. 特殊不带 -free 的免费模型 big-pickle，始终保持原样，绝不加 -free
		{"big-pickle", "big-pickle"},
		{"big-pickle-free", "big-pickle"},
		// 4. 简写别名兼容
		{"muse-spark-1.3", "muse-spark-1.3-contributor-free"},
	}

	for _, tc := range testCases {
		body := []byte(`{"model":"` + tc.input + `","messages":[{"role":"user","content":"hi"}]}`)
		rewritten := gw.rewriteModel(context.Background(), body)
		var payload map[string]any
		if err := json.Unmarshal(rewritten, &payload); err != nil {
			t.Fatalf("failed to unmarshal rewritten for %s: %v", tc.input, err)
		}
		if payload["model"] != tc.expected {
			t.Fatalf("for input %q expected model to be %q, got %q", tc.input, tc.expected, payload["model"])
		}
	}
}
