package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const modelCacheTTL = 60 * time.Second

type cachedModels struct {
	rename   map[string]string
	redirect map[string]string
	loadedAt time.Time
}

// defaultModelMaps 基础免费模型兜底映射，防止首次启动若遇上游网络抖动拉取失败导致映射为空引发 401 ModelError。
var defaultModelMaps = map[string]string{
	"muse-spark-1.3-contributor-free": "muse-spark-1.3-contributor",
	"muse-spark-1.2-contributor-free": "muse-spark-1.2-contributor",
	"deepseek-v4-flash-free":          "deepseek-v4-flash",
	"big-pickle":                      "big-pickle",
	"jev-1.13-free":                   "jev-1.13",
	"mimo-v2.5-free":                  "mimo-v2.5",
	"ling-3.0-flash-fin-free":         "ling-3.0-flash-fin",
	"nemotron-3-ultra-free":           "nemotron-3-ultra",
	"nemotron-3.5-lightning-free":     "nemotron-3.5-lightning",
}

func (g *gateway) modelMaps(ctx context.Context) (map[string]string, map[string]string) {
	g.modelMu.Lock()
	defer g.modelMu.Unlock()

	if g.modelCache != nil && time.Since(g.modelCache.loadedAt) < modelCacheTTL {
		return cloneStringMap(g.modelCache.rename), cloneStringMap(g.modelCache.redirect)
	}

	rename, err := g.fetchModelMaps(ctx)
	if err != nil {
		if g.modelCache != nil {
			log.Printf("[模型] 刷新失败，使用缓存: %v", err)
			g.modelCache.loadedAt = time.Now().Add(-(modelCacheTTL - 5*time.Second))
			return cloneStringMap(g.modelCache.rename), cloneStringMap(g.modelCache.redirect)
		}
		log.Printf("[模型] 刷新失败且无缓存，使用内置兜底映射: %v", err)
		rename = cloneStringMap(defaultModelMaps)
		redirect := buildRedirect(rename)
		// 短暂缓存兜底结果，避免上游故障时并发请求在互斥锁后逐个等待 8 秒。
		g.modelCache = &cachedModels{
			rename:   cloneStringMap(rename),
			redirect: cloneStringMap(redirect),
			loadedAt: time.Now().Add(-(modelCacheTTL - 5*time.Second)),
		}
		return rename, redirect
	}

	redirect := buildRedirect(rename)
	g.modelCache = &cachedModels{rename: rename, redirect: redirect, loadedAt: time.Now()}
	log.Printf("[模型] 已刷新 %d 个免费模型", len(rename))
	return cloneStringMap(rename), cloneStringMap(redirect)
}

func (g *gateway) fetchModelMaps(parent context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()

	target := strings.TrimRight(g.cfg.project.upstream, "/") + g.cfg.project.modelPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", opencodeUserAgent())
	req.Header.Set("X-Opencode-Client", "cli")
	if auth := g.cfg.project.upstreamAuthorization; auth != "" {
		req.Header.Set("Authorization", auth)
	}

	client := &http.Client{Transport: controlTransport(8 * time.Second)}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("model API returned %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	ids, err := extractModelIDs(body)
	if err != nil {
		return nil, err
	}

	rename := make(map[string]string)
	for _, id := range ids {
		switch g.cfg.project.modelMode {
		case modelKilo:
			if !strings.HasSuffix(id, ":free") {
				continue
			}
			trimmed := strings.TrimSuffix(id, ":free")
			parts := strings.Split(trimmed, "/")
			rename[id] = parts[len(parts)-1]
		case modelOpenCode:
			if strings.HasSuffix(id, "-free") {
				rename[id] = strings.TrimSuffix(id, "-free")
			} else if id == "big-pickle" {
				rename[id] = id
			}
		}
	}
	return rename, nil
}

func extractModelIDs(body []byte) ([]string, error) {
	var root any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if object, ok := root.(map[string]any); ok {
		root = object["data"]
	}
	items, ok := root.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected model response")
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		switch value := item.(type) {
		case string:
			ids = append(ids, value)
		case map[string]any:
			if id, ok := value["id"].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (g *gateway) modelsResponse(ctx context.Context) *gatewayResponse {
	rename, _ := g.modelMaps(ctx)
	unique := make(map[string]struct{}, len(rename)+len(g.cfg.project.extraModels))
	for _, display := range rename {
		unique[display] = struct{}{}
	}
	for _, model := range g.cfg.project.extraModels {
		unique[model] = struct{}{}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	created := time.Now().Unix()
	models := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		models = append(models, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": g.cfg.project.ownedBy,
		})
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": models})
	return &gatewayResponse{
		status: http.StatusOK,
		header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		body:   body,
	}
}

func (g *gateway) rewriteModel(ctx context.Context, body []byte) []byte {
	_, redirect := g.modelMaps(ctx)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	model, _ := payload["model"].(string)
	if model == "" {
		return body
	}

	upstream, exists := redirect[model]
	if !exists {
		cleanModel := strings.TrimPrefix(model, "oc/")
		cleanModel = strings.TrimPrefix(cleanModel, "opencode/")
		if target, ok := redirect[cleanModel]; ok {
			upstream = target
			exists = true
		} else if g.cfg.project.modelMode == modelOpenCode && !strings.HasSuffix(model, "-free") && model != "big-pickle" {
			log.Printf("[模型提示] 请求模型 %q 尚未在已知免费字典中，自动追加 -free 规避 403", model)
			upstream = model + "-free"
			exists = true
		} else {
			log.Printf("[模型警告] 请求模型 %q 不在免费映射表中，上游可能判定越权返回 403 FreeTierError", model)
			return body
		}
	}
	if upstream == model {
		return body
	}
	payload["model"] = upstream
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	log.Printf("[模型重定向] %s -> %s", model, upstream)
	return rewritten
}

func buildRedirect(rename map[string]string) map[string]string {
	redirect := make(map[string]string, len(rename)*2+4)
	for upstream, display := range rename {
		// 1. 展示名称（如去除 -free 后的名称）映射回真实上游名称（如带 -free 的名称）
		if display != "" {
			redirect[display] = upstream
		}
		// 2. 客户端若本身就传入带 -free 的真实上游名称，保持不变
		redirect[upstream] = upstream
	}
	// 3. 常见客户端别名兼容：例如客户端常传 muse-spark-1.3 省略 contributor
	if target, ok := redirect["muse-spark-1.3-contributor"]; ok {
		redirect["muse-spark-1.3"] = target
	}
	if target, ok := redirect["muse-spark-1.2-contributor"]; ok {
		redirect["muse-spark-1.2"] = target
	}
	// 4. 若客户端误传 big-pickle-free，纠正为真实名称 big-pickle
	if _, ok := redirect["big-pickle"]; ok {
		redirect["big-pickle-free"] = "big-pickle"
	}
	return redirect
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
