package config

import "testing"

func TestQuickstartHTTPRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("AEGISOPS_PLATFORM_AUTH_ALLOW_QUICKSTART_HTTP", "true")
	cfg, err := Load(LoadOptions{NoEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Platform.Auth.AllowQuickstartHTTP {
		t.Fatal("production default permits insecure cookies")
	}
	cfg, err = Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Platform.Auth.AllowQuickstartHTTP {
		t.Fatal("explicit environment opt-in was ignored")
	}
	cfg, err = Load(LoadOptions{Overrides: map[string]any{"platform.auth.allow_quickstart_http": false}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Platform.Auth.AllowQuickstartHTTP {
		t.Fatal("CLI override must take precedence")
	}
}
