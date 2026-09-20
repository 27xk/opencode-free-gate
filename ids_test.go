package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestOpencodeIDFormatAndGeneration(t *testing.T) {
	msg := globalIDGen.generate("msg", false)
	if !isValidOpencodeID(msg, "msg") {
		t.Fatalf("generated request id should be valid opencode msg format: %q", msg)
	}
	if len(msg) != 30 {
		t.Fatalf("expected length 30, got %d for %q", len(msg), msg)
	}

	ses := globalIDGen.generate("ses", true)
	if !isValidOpencodeID(ses, "ses") {
		t.Fatalf("generated session id should be valid opencode ses format: %q", ses)
	}
	if len(ses) != 30 {
		t.Fatalf("expected length 30, got %d for %q", len(ses), ses)
	}
}

func TestIsValidOpencodeID(t *testing.T) {
	validMsg := "msg_0b20bce72001HhNaOVex180aKz"
	validSes := "ses_f4df4cb3bffetj5bTDp2vZHLRH"
	if !isValidOpencodeID(validMsg, "msg") {
		t.Fatalf("expected %q to be valid msg", validMsg)
	}
	if !isValidOpencodeID(validSes, "ses") {
		t.Fatalf("expected %q to be valid ses", validSes)
	}

	invalid := []string{
		"",
		"msg_",
		"msg_123",
		"req_0b20bce72001HhNaOVex180aKz",
		"msg_0b20bce72001HhNaOVex180aK!", // invalid char
		"ses_f4df4cb3bffetj5bTDp2vZHLRHxxx", // too long
	}
	for _, id := range invalid {
		if isValidOpencodeID(id, "msg") {
			t.Fatalf("expected %q to be invalid msg", id)
		}
	}
}

func TestDeriveRequestIDsStableAcrossTurns(t *testing.T) {
	turn1 := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello world"},
		},
	}
	turn2 := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello world"},
			map[string]any{"role": "assistant", "content": "hi"},
			map[string]any{"role": "user", "content": "second turn"},
		},
	}
	first := deriveRequestIDs(http.Header{}, turn1)
	second := deriveRequestIDs(http.Header{}, turn2)
	if first.Session != second.Session {
		t.Fatalf("same conversation must keep one session: %q vs %q", first.Session, second.Session)
	}
	if first.Request == second.Request {
		t.Fatal("each client request must get a fresh request id")
	}

	other := deriveRequestIDs(http.Header{}, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "different opener"}},
	})
	if other.Session == first.Session {
		t.Fatal("different conversations must not share a session")
	}
}

func TestDeriveRequestIDsHonorsExplicitSession(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Session-Id", "session-a")
	bodyA := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "same opener"}}}
	bodyB := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "same opener"}}}

	withHeader := deriveRequestIDs(headers, bodyA)
	otherHeader := http.Header{}
	otherHeader.Set("X-Session-Id", "session-b")
	withOther := deriveRequestIDs(otherHeader, bodyB)
	if withHeader.Session == withOther.Session {
		t.Fatal("explicit session ids must separate identical openers")
	}
	if !isValidOpencodeID(withHeader.Session, "ses") {
		t.Fatalf("session id must use the opencode format: %q", withHeader.Session)
	}
	if !isValidOpencodeID(withHeader.Request, "msg") || withHeader.Project != "global" {
		t.Fatalf("unexpected id formats: %q %q", withHeader.Request, withHeader.Project)
	}

	// Test preserving legitimate OpenCode session ID
	opencodeSes := "ses_f4df4cb3bffetj5bTDp2vZHLRH"
	headersOC := http.Header{}
	headersOC.Set("x-opencode-session", opencodeSes)
	headersOC.Set("x-opencode-project", "my-project")
	withOC := deriveRequestIDs(headersOC, bodyA)
	if withOC.Session != opencodeSes {
		t.Fatalf("valid opencode session should be preserved, got %q", withOC.Session)
	}
	if withOC.Project != "my-project" {
		t.Fatalf("custom project should be preserved, got %q", withOC.Project)
	}
}

func TestDeriveRequestIDsResponsesInput(t *testing.T) {
	viaInput := deriveRequestIDs(http.Header{}, map[string]any{"input": "codex prompt"})
	again := deriveRequestIDs(http.Header{}, map[string]any{"input": "codex prompt"})
	if viaInput.Session != again.Session {
		t.Fatal("responses input string must yield a stable session")
	}

	viaPrevious := deriveRequestIDs(http.Header{}, map[string]any{"previous_response_id": "resp_123"})
	againPrevious := deriveRequestIDs(http.Header{}, map[string]any{"previous_response_id": "resp_123"})
	if viaPrevious.Session != againPrevious.Session {
		t.Fatal("previous_response_id must yield a stable session")
	}
}

func TestEnsureStreamAndTools(t *testing.T) {
	// Case 1: Empty tools for OpenAI path (/v1/chat/completions)
	payload1 := map[string]any{
		"model": "muse-spark-1.3",
	}
	ensureStreamAndTools("/v1/chat/completions", payload1)
	if stream, _ := payload1["stream"].(bool); !stream {
		t.Fatal("stream must be true")
	}
	tools1, ok := payload1["tools"].([]any)
	if !ok || len(tools1) < 2 {
		t.Fatalf("expected at least 2 tools, got %v", payload1["tools"])
	}
	// Verify OpenAI standard format: {"type":"function", "function": {"name": "bash", ...}}
	tool0, _ := tools1[0].(map[string]any)
	if tool0["type"] != "function" {
		t.Fatalf("expected tool type function, got %v", tool0["type"])
	}
	fn0, ok := tool0["function"].(map[string]any)
	if !ok || fn0["name"] != "bash" {
		t.Fatalf("expected function.name bash, got %v", fn0)
	}

	// Case 2: Non-standard flat tool converted to standard OpenAI format
	payload2 := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "custom_search"},
		},
	}
	ensureStreamAndTools("/v1/chat/completions", payload2)
	tools2 := payload2["tools"].([]any)
	if len(tools2) < 2 {
		t.Fatalf("expected at least 2 tools, got %d", len(tools2))
	}
	hasBash := false
	for _, tool := range tools2 {
		m := tool.(map[string]any)
		if fn, ok := m["function"].(map[string]any); ok && fn["name"] == "bash" {
			hasBash = true
		}
	}
	if !hasBash {
		t.Fatal("expected bash tool with nested function object to be injected")
	}

	// Case 3: Anthropic path (/v1/messages)
	payload3 := map[string]any{
		"model": "muse-spark-1.3",
	}
	ensureStreamAndTools("/v1/messages", payload3)
	tools3 := payload3["tools"].([]any)
	if len(tools3) < 2 {
		t.Fatalf("expected at least 2 tools for Anthropic, got %d", len(tools3))
	}
	toolAnthropic0, _ := tools3[0].(map[string]any)
	if toolAnthropic0["name"] != "bash" || toolAnthropic0["input_schema"] == nil {
		t.Fatalf("expected Anthropic tool format with input_schema, got %v", toolAnthropic0)
	}

	// Case 4: Many tools provided without 'read', ensure 'read' is unconditionally added
	payload4 := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "tool_1"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "tool_2"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
		},
	}
	ensureStreamAndTools("/v1/chat/completions", payload4)
	tools4 := payload4["tools"].([]any)
	hasBash4 := false
	hasRead4 := false
	for _, tool := range tools4 {
		m := tool.(map[string]any)
		if fn, ok := m["function"].(map[string]any); ok {
			if fn["name"] == "bash" {
				hasBash4 = true
			}
			if fn["name"] == "read" {
				hasRead4 = true
			}
		}
	}
	if !hasBash4 || !hasRead4 {
		t.Fatalf("expected both bash and read to be present, got hasBash=%v, hasRead=%v", hasBash4, hasRead4)
	}
}

func TestOpencodeUserAgent(t *testing.T) {
	orig := os.Getenv("OPENCODE_USER_AGENT")
	defer os.Setenv("OPENCODE_USER_AGENT", orig)

	os.Unsetenv("OPENCODE_USER_AGENT")
	if !strings.HasPrefix(opencodeUserAgent(), "opencode/1.18.31") {
		t.Fatalf("expected default UA to match opencode/1.18.31, got %q", opencodeUserAgent())
	}

	os.Setenv("OPENCODE_USER_AGENT", "custom-agent/1.0")
	if opencodeUserAgent() != "custom-agent/1.0" {
		t.Fatalf("expected custom UA, got %q", opencodeUserAgent())
	}
}

func TestApplyAnthropicAuth(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer public")
	applyAnthropicAuth(headers)
	if headers.Get("Authorization") != "" {
		t.Fatal("anthropic requests must not carry Authorization")
	}
	if got := headers.Get("X-Api-Key"); got != "public" {
		t.Fatalf("anthropic requests must use x-api-key: %q", got)
	}
	if got := headers.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("missing default anthropic-version: %q", got)
	}

	custom := http.Header{}
	custom.Set("Authorization", "Bearer public")
	custom.Set("Anthropic-Version", "2024-01-01")
	applyAnthropicAuth(custom)
	if got := custom.Get("Anthropic-Version"); got != "2024-01-01" {
		t.Fatalf("client anthropic-version must be preserved: %q", got)
	}
}

func TestSessionAffinityPrefersStableSlot(t *testing.T) {
	gw := newGateway(config{})
	for _, address := range []string{"proxy-1", "proxy-2", "proxy-3", "proxy-4", "proxy-5"} {
		gw.slots = append(gw.slots, slot{addr: address})
	}

	first, ok := gw.nextSlot(false, map[string]struct{}{}, "ses_alpha", 0)
	if !ok {
		t.Fatal("expected a slot for the session")
	}
	for range 10 {
		again, ok := gw.nextSlot(false, map[string]struct{}{}, "ses_alpha", 0)
		if !ok || again.addr != first.addr {
			t.Fatalf("session must stick to %s, got %s", first.addr, again.addr)
		}
	}

	tried := map[string]struct{}{first.addr: {}}
	fallback, ok := gw.nextSlot(false, tried, "ses_alpha", 1)
	if !ok || fallback.addr == first.addr {
		t.Fatalf("failed slot must be skipped, got %s", fallback.addr)
	}

	distinct := make(map[string]struct{})
	for _, session := range []string{"ses_a", "ses_b", "ses_c", "ses_d", "ses_e", "ses_f", "ses_g", "ses_h"} {
		selected, ok := gw.nextSlot(false, map[string]struct{}{}, session, 0)
		if !ok {
			t.Fatal("expected a slot")
		}
		distinct[selected.addr] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatal("different sessions should spread across slots")
	}
}
