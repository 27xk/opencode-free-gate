package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	base62Chars      = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	defaultUserAgent = "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
)

type requestIDs struct {
	Session string
	Request string
	Project string
}

type idGenerator struct {
	mu   sync.Mutex
	last int64
	ctr  int64
}

var globalIDGen idGenerator

// generate 按照 OpenCode 官方客户端算法生成 K-sortable 唯一标识。
// - prefix: 前缀，如 "msg" 或 "ses"
// - desc: 降序标识（ses 使用 true，按位取反；msg 使用 false）
// 格式为：prefix_ + 12 位时间戳 Hex + 14 位 Base62 随机字符，总长 30 字符。
func (g *idGenerator) generate(prefix string, desc bool) string {
	g.mu.Lock()
	now := time.Now().UnixMilli()
	if now != g.last {
		g.last = now
		g.ctr = 1
	} else {
		g.ctr++
	}
	ts := g.last
	ctr := g.ctr
	g.mu.Unlock()

	// v 为 48 位 (6 字节) 整数：(ts * 0x1000) + ctr
	v := (uint64(ts) * 0x1000) + uint64(ctr)

	var timeBytes [6]byte
	for i := 0; i < 6; i++ {
		shift := 40 - 8*i
		b := byte((v >> shift) & 0xff)
		if desc {
			b = ^b
		}
		timeBytes[i] = b
	}
	timeHex := hex.EncodeToString(timeBytes[:])

	rndBytes := make([]byte, 14)
	if _, err := rand.Read(rndBytes); err != nil {
		for i := range rndBytes {
			rndBytes[i] = byte(time.Now().UnixNano() + int64(i))
		}
	}
	var rndStr [14]byte
	for i := 0; i < 14; i++ {
		rndStr[i] = base62Chars[rndBytes[i]%62]
	}

	return prefix + "_" + timeHex + string(rndStr[:])
}

// isValidOpencodeID 校验是否为合规的 OpenCode ID 格式 (如 ses_... 或 msg_...)
func isValidOpencodeID(id, expectedPrefix string) bool {
	prefix := expectedPrefix + "_"
	if !strings.HasPrefix(id, prefix) || len(id) != 30 {
		return false
	}
	// 4..16 为 12 位十六进制
	for _, c := range id[len(prefix) : len(prefix)+12] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	// 16..30 为 14 位 Base62 字符
	for _, c := range id[len(prefix)+12:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

type sessionCacheEntry struct {
	sessionID string
	updatedAt time.Time
}

type sessionCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	entries map[string]sessionCacheEntry
}

func newSessionCache(ttl time.Duration, maxSize int) *sessionCache {
	return &sessionCache{
		ttl:     ttl,
		maxSize: maxSize,
		entries: make(map[string]sessionCacheEntry),
	}
}

func (c *sessionCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Since(entry.updatedAt) > c.ttl {
		delete(c.entries, key)
		return "", false
	}
	entry.updatedAt = time.Now()
	c.entries[key] = entry
	return entry.sessionID, true
}

func (c *sessionCache) set(key, sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.maxSize {
		now := time.Now()
		for k, v := range c.entries {
			if now.Sub(v.updatedAt) > c.ttl {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.maxSize {
			count := 0
			toRemove := c.maxSize / 5
			for k := range c.entries {
				delete(c.entries, k)
				count++
				if count >= toRemove {
					break
				}
			}
		}
	}
	c.entries[key] = sessionCacheEntry{
		sessionID: sessionID,
		updatedAt: time.Now(),
	}
}

var globalSessionCache = newSessionCache(24*time.Hour, 10000)

// deriveRequestIDs 生成上游 OpenCode 协议所需的会话、请求与项目标识。
// 会话 ID 优先保留客户端传入的合法 ses_ 格式标识；若为其他标识或根据多轮首条消息派生，
// 则通过会话缓存映射到合规且稳定的 ses_ 标识，保证多轮对话始终映射到同一会话与代理出口。
func deriveRequestIDs(headers http.Header, body map[string]any) requestIDs {
	explicitSession := firstString(
		headers.Get("x-opencode-session"),
		headers.Get("x-session-id"),
		headers.Get("conversation-id"),
		stringAt(body, "conversation_id"),
		stringAt(body, "metadata", "session_id"),
	)

	var session string
	if explicitSession != "" && isValidOpencodeID(explicitSession, "ses") {
		session = explicitSession
	} else {
		signal := explicitSession
		if signal == "" {
			signal = conversationSeed(body)
		}
		if signal == "" {
			signal = stringAt(body, "previous_response_id")
		}
		if signal == "" || signal == "{}" {
			signal = globalIDGen.generate("ses", true)
		}

		if cached, ok := globalSessionCache.get(signal); ok {
			session = cached
		} else {
			session = globalIDGen.generate("ses", true)
			globalSessionCache.set(signal, session)
		}
	}

	request := headers.Get("x-opencode-request")
	if request == "" || !isValidOpencodeID(request, "msg") {
		request = globalIDGen.generate("msg", false)
	}

	project := firstString(headers.Get("x-opencode-project"), stringAt(body, "metadata", "project_id"))
	if project == "" {
		project = "global"
	}

	return requestIDs{
		Session: session,
		Request: request,
		Project: project,
	}
}

func conversationSeed(body map[string]any) string {
	if input, ok := body["input"].(string); ok && input != "" {
		return input
	}
	for _, field := range []string{"messages", "input"} {
		items, _ := body[field].([]any)
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok || stringAt(item, "role") != "user" {
				continue
			}
			encoded, _ := json.Marshal(item["content"])
			if len(encoded) > 0 && string(encoded) != "null" {
				return string(encoded)
			}
		}
	}
	return ""
}

func firstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func stringAt(value map[string]any, path ...string) string {
	current := any(value)
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	result, _ := current.(string)
	return result
}

func opencodeUserAgent() string {
	if ua := strings.TrimSpace(os.Getenv("OPENCODE_USER_AGENT")); ua != "" {
		return ua
	}
	return defaultUserAgent
}

func getToolName(tool any) string {
	m, ok := tool.(map[string]any)
	if !ok {
		return ""
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok && name != "" {
			return name
		}
	}
	if name, ok := m["name"].(string); ok && name != "" {
		return name
	}
	return ""
}

func makeOpenAITool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Execute " + name,
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func makeAnthropicTool(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "Execute " + name,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

// ensureStreamAndTools 确保发往上游 OpenCode 的请求体满足免费模型最新风控要求：
// 1. stream: true
// 2. tools 数组里必须有 name=bash 的项，且 tools 数量 >= 2
// 严格对齐协议格式（OpenAI 下 tools 必须包含 function 嵌套对象，符合 SGLang/Pydantic 校验；Anthropic 下包含 input_schema）
func ensureStreamAndTools(path string, payload map[string]any) {
	if payload == nil {
		return
	}
	payload["stream"] = true

	isAnthropic := strings.HasPrefix(path, "/v1/messages")
	rawTools, _ := payload["tools"].([]any)
	hasBash := false
	hasRead := false
	normalizedTools := make([]any, 0, len(rawTools)+2)

	for _, item := range rawTools {
		m, ok := item.(map[string]any)
		if !ok {
			normalizedTools = append(normalizedTools, item)
			continue
		}
		name := getToolName(m)
		if name == "bash" {
			hasBash = true
		}
		if name == "read" {
			hasRead = true
		}

		if !isAnthropic {
			// 如果是 OpenAI 兼容接口，且工具缺少标准 function 嵌套对象，则自动包装为标准格式
			if _, hasFn := m["function"].(map[string]any); !hasFn && name != "" {
				desc, _ := m["description"].(string)
				if desc == "" {
					desc = "Execute " + name
				}
				params, ok := m["parameters"].(map[string]any)
				if !ok || params == nil {
					params = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				normalizedTools = append(normalizedTools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        name,
						"description": desc,
						"parameters":  params,
					},
				})
				continue
			}
		}
		normalizedTools = append(normalizedTools, m)
	}

	if !hasBash {
		if isAnthropic {
			normalizedTools = append(normalizedTools, makeAnthropicTool("bash"))
		} else {
			normalizedTools = append(normalizedTools, makeOpenAITool("bash"))
		}
	}
	if !hasRead {
		if isAnthropic {
			normalizedTools = append(normalizedTools, makeAnthropicTool("read"))
		} else {
			normalizedTools = append(normalizedTools, makeOpenAITool("read"))
		}
	}
	payload["tools"] = normalizedTools
}
