package config

import "testing"

func TestExecPodLimits(t *testing.T) {
	for _, kind := range []string{"defaults", "disabled", "timeout", "output", "concurrency"} {
		t.Run(kind, func(t *testing.T) {
			cfg, err := Load(LoadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "disabled":
				cfg.Agent.ExecPod.Enabled = false
			case "timeout":
				cfg.Agent.ExecPod.MaxTimeoutSeconds = 31
			case "output":
				cfg.Agent.ExecPod.MaxOutputBytes = 65537
			case "concurrency":
				cfg.Agent.ExecPod.MaxConcurrentPerUser = 3
			}
			err = cfg.Validate()
			if (err == nil) != (kind == "defaults" || kind == "disabled") {
				t.Fatalf("%s: %v", kind, err)
			}
		})
	}
}
