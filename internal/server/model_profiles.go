package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/settings"
)

type modelProfiles struct {
	load func(context.Context) (*config.Config, error)
	mu   sync.Mutex
}

func (s *Server) WithModelProfiles(load func(context.Context) (*config.Config, error)) *Server {
	s.modelProfiles.load = load
	return s
}

type modelProfileFields struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Endpoint      string `json:"endpoint"`
	APIFormat     string `json:"apiFormat"`
	ContextWindow int    `json:"contextWindow"`
	MaxTokens     int    `json:"maxTokens"`
}

func (s *Server) handleModelProfiles(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil || s.modelProfiles.load == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "未启用独立模型配置")
		return
	}
	if r.Method != http.MethodGet {
		s.modelProfiles.mu.Lock()
		defer s.modelProfiles.mu.Unlock()
	}
	cfg, err := s.modelProfiles.load(r.Context())
	if err != nil || cfg == nil {
		writeError(w, r, http.StatusServiceUnavailable, codeInternal, "无法读取模型配置，请检查服务端配置")
		return
	}
	profiles := append([]config.ModelProfile{}, cfg.LLM.ModelProfiles...)
	if r.Method == http.MethodGet {
		type view struct {
			modelProfileFields
			ID     string `json:"id"`
			HasKey bool   `json:"hasKey"`
		}
		rows := make([]view, 0, len(profiles))
		for _, profile := range profiles {
			rows = append(rows, view{ID: profile.ID, HasKey: profile.APIKey != "", modelProfileFields: modelProfileFields{
				Name: profile.Name, Model: profile.Model, Endpoint: profile.Endpoint, APIFormat: profile.APIFormat,
				ContextWindow: profile.ContextWindow, MaxTokens: profile.MaxTokens,
			}})
		}
		writeOK(w, r, http.StatusOK, rows)
		return
	}
	id, index := r.PathValue("id"), -1
	for idx, profile := range profiles {
		if profile.ID == id {
			index = idx
			break
		}
	}
	if r.Method != http.MethodPost && index < 0 {
		writeError(w, r, http.StatusNotFound, codeNotFound, "模型不存在")
		return
	}
	if r.Method == http.MethodDelete {
		profiles = append(profiles[:index], profiles[index+1:]...)
	} else {
		var input struct {
			modelProfileFields
			APIKey *string `json:"apiKey"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "模型配置格式不正确")
			return
		}
		profile := config.ModelProfile{
			ID: id, Name: strings.TrimSpace(input.Name), Model: strings.TrimSpace(input.Model),
			Endpoint: strings.TrimSpace(input.Endpoint), APIFormat: input.APIFormat,
			ContextWindow: input.ContextWindow, MaxTokens: input.MaxTokens,
		}
		if input.APIKey != nil {
			profile.APIKey = strings.TrimSpace(*input.APIKey)
		} else if index >= 0 {
			if profiles[index].Endpoint != profile.Endpoint && profiles[index].APIKey != "" {
				writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "修改接口地址时请重新填写或清除 API Key")
				return
			}
			profile.APIKey = profiles[index].APIKey
		}
		if index < 0 {
			profile.ID = "profile_" + rand.Text()
			id = profile.ID
			profiles = append(profiles, profile)
		} else {
			profiles[index] = profile
		}
	}
	if err := config.ValidateModelProfiles(profiles); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, err.Error())
		return
	}
	raw, _ := json.Marshal(profiles)
	actor := settings.Actor{RequestID: RequestIDFrom(r.Context())}
	if user := UserFrom(r.Context()); user != nil {
		actor.UserID = user.ID
	}
	if _, err := s.settings.PatchGroup(r.Context(), settings.GroupModel, map[string]json.RawMessage{"model_profiles": raw}, actor); err != nil {
		writeSettingsError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]string{"id": id})
}
