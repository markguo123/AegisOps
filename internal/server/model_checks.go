package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/mcp"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type modelChecks struct {
	active    sync.Mutex
	mcpActive sync.Mutex
	load      func(context.Context) (*config.Config, error)
}

func (s *Server) handleMCPCheck(w http.ResponseWriter, r *http.Request) {
	if s.modelChecks.load == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "未启用 MCP 检测")
		return
	}
	if !s.modelChecks.mcpActive.TryLock() {
		writeError(w, r, http.StatusTooManyRequests, codeInvalidArgument, "MCP 检测正在进行")
		return
	}
	defer s.modelChecks.mcpActive.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cfg, err := s.modelChecks.load(ctx)
	if err != nil || cfg == nil || !cfg.MCP.Enabled || len(cfg.MCP.Servers) == 0 {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "请先保存并启用 MCP 配置")
		return
	}
	type status struct {
		Index int    `json:"index"`
		Tools int    `json:"tools"`
		Error string `json:"error,omitempty"`
	}
	results := make([]status, 0, len(cfg.MCP.Servers))
	for i, spec := range cfg.MCP.Servers {
		item := status{Index: i + 1}
		registry := tools.NewRegistry()
		closeRemote, err := mcp.RegisterRemote(ctx, registry, spec)
		if err != nil {
			item.Error = err.Error()
		} else {
			item.Tools = len(registry.All())
			closeRemote()
		}
		results = append(results, item)
	}
	writeOK(w, r, http.StatusOK, results)
}

func (s *Server) WithModelChecks(load func(context.Context) (*config.Config, error)) *Server {
	s.modelChecks.load = load
	return s
}

func (s *Server) handleModelCheck(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "discover" && action != "probe" {
		writeError(w, r, http.StatusNotFound, codeNotFound, "未知模型操作")
		return
	}
	checks := &s.modelChecks
	if checks.load == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "未启用模型检测")
		return
	}
	if !checks.active.TryLock() {
		writeError(w, r, http.StatusTooManyRequests, codeInvalidArgument, "另一个模型检测正在进行")
		return
	}
	defer checks.active.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cfg, err := checks.load(ctx)
	if err != nil || cfg == nil || !cfg.LLM.Enabled {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "请先保存并启用服务端模型配置")
		return
	}
	var input struct {
		ProfileID string  `json:"profileId"`
		Endpoint  string  `json:"endpoint"`
		APIFormat string  `json:"apiFormat"`
		APIKey    *string `json:"apiKey"`
		Model     string  `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&input); err != nil || input.APIFormat != "openai-chat-completions" {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "无效的模型检测参数")
		return
	}
	key := ""
	if input.ProfileID != "" {
		found := false
		for _, profile := range cfg.LLM.ModelProfiles {
			if profile.ID != input.ProfileID {
				continue
			}
			found = true
			if input.APIKey == nil {
				if profile.Endpoint != strings.TrimSpace(input.Endpoint) && profile.APIKey != "" {
					writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "修改接口地址时请重新填写或清除 API Key")
					return
				}
				key = profile.APIKey
			}
			break
		}
		if !found {
			writeError(w, r, http.StatusNotFound, codeNotFound, "模型不存在")
			return
		}
	}
	if input.APIKey != nil {
		key = strings.TrimSpace(*input.APIKey)
	}
	endpoint, err := url.Parse(strings.TrimSpace(input.Endpoint))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "模型接口地址配置无效")
		return
	}
	method, path := http.MethodGet, "models"
	var body io.Reader
	if action == "probe" {
		if len(input.Model) > 256 || strings.TrimSpace(input.Model) == "" {
			writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "请选择要测试的模型")
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"model": strings.TrimSpace(input.Model), "max_tokens": 32,
			"messages": []map[string]string{{"role": "user", "content": "Reply with OK."}},
		})
		method, path, body = http.MethodPost, "chat/completions", bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.JoinPath(path).String(), body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "模型接口地址配置无效")
		return
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, codeInternal, "模型接口不可达或检测超时")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, r, http.StatusBadGateway, codeInternal, "模型接口拒绝请求，请检查凭据、模型名或接口能力")
		return
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil ||
		(action == "discover" && result.Data == nil) ||
		(action == "probe" && (len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "")) {
		writeError(w, r, http.StatusBadGateway, codeInternal, "模型接口未返回预期结果")
		return
	}
	models := make([]string, 0)
	seen := make(map[string]bool)
	for _, entry := range result.Data {
		id := strings.TrimSpace(entry.ID)
		if id != "" && len(id) <= 256 && !seen[id] && (key == "" || !strings.Contains(id, key)) {
			models = append(models, id)
			seen[id] = true
		}
		if len(models) == 200 {
			break
		}
	}
	sort.Strings(models)
	writeOK(w, r, http.StatusOK, map[string]any{"models": models, "checkedAt": time.Now().UTC()})
}
