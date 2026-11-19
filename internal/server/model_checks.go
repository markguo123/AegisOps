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
)

type modelChecks struct {
	load func(context.Context) (*config.Config, error)
	mu   sync.Mutex
	next time.Time
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
	checks.mu.Lock()
	now := time.Now()
	if now.Before(checks.next) {
		checks.mu.Unlock()
		writeError(w, r, http.StatusTooManyRequests, codeInvalidArgument, "模型检测过于频繁，请稍后再试")
		return
	}
	checks.next = now.Add(8 * time.Second)
	checks.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cfg, err := checks.load(ctx)
	if err != nil || cfg == nil || !cfg.LLM.Enabled {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "请先保存并启用服务端模型配置")
		return
	}
	endpoint, err := url.Parse(cfg.LLM.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "模型接口地址配置无效")
		return
	}
	method, path := http.MethodGet, "models"
	var body io.Reader
	if action == "probe" {
		var input struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil ||
			len(input.Model) > 256 || strings.TrimSpace(input.Model) == "" {
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
	if cfg.LLM.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.LLM.APIKey)
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
		if id != "" && len(id) <= 256 && !seen[id] && (cfg.LLM.APIKey == "" || !strings.Contains(id, cfg.LLM.APIKey)) {
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
