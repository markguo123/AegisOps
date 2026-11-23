package service

import (
	"errors"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/config"
)

func TestResolveIndependentModelProfiles(t *testing.T) {
	base := &config.Config{}
	base.LLM.Model = "scan-default"
	base.LLM.Endpoint = "https://scan.example/v1"
	base.LLM.ModelProfiles = []config.ModelProfile{
		{ID: "profile_a", Name: "私有 A", Model: "shared-name", Endpoint: "https://a.example/v1", APIKey: "key-a", APIFormat: config.ModelAPIChatCompletions, ContextWindow: 131072, MaxTokens: 4096},
		{ID: "profile_b", Name: "私有 B", Model: "shared-name", Endpoint: "https://b.example/v1", APIKey: "key-b", APIFormat: config.ModelAPIChatCompletions, ContextWindow: 65536, MaxTokens: 2048},
	}
	for _, profile := range base.LLM.ModelProfiles {
		cfg, selected, err := ResolveConfiguredModel(base, profile.ID)
		if err != nil {
			t.Fatal(err)
		}
		if selected != profile.ID || cfg.LLM.Model != profile.Model || cfg.LLM.Endpoint != profile.Endpoint ||
			cfg.LLM.APIKey != profile.APIKey || cfg.LLM.MaxTokens != profile.MaxTokens || cfg.Agent.ContextWindow != profile.ContextWindow {
			t.Fatalf("wrong configuration for %s", profile.ID)
		}
	}
	if base.LLM.Model != "scan-default" || base.LLM.Endpoint != "https://scan.example/v1" {
		t.Fatal("chat selection changed global configuration")
	}
	if catalog := configuredChatModels(base); len(catalog) != 2 || catalog[0].Name != "私有 A" {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if _, selected, err := ResolveConfiguredModel(base, ""); selected != "profile_a" || err != nil {
		t.Fatalf("expected first independent model: %s, %v", selected, err)
	}
	if _, _, err := ResolveConfiguredModel(base, "profile_removed"); !errors.Is(err, ErrChatModelUnavailable) {
		t.Fatalf("unavailable model must not fall back: %v", err)
	}

	base.LLM.DefaultChatModel = "profile_b"
	resolved, id, err := ResolveConfiguredModel(base, "")
	if err != nil || id != "profile_b" || resolved.Agent.ContextWindow != 65536 || configuredChatModels(base)[0].ID != id {
		t.Fatalf("configured default was not applied: %q, %v", id, err)
	}
}
