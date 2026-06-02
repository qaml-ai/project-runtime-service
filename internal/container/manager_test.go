package container

import (
	"testing"
	"time"
)

func TestContainerIdleTimeoutFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		preferred   string
		legacy      string
		wantTimeout time.Duration
	}{
		{
			name:        "default is five minutes",
			wantTimeout: 5 * time.Minute,
		},
		{
			name:        "preferred env wins over legacy env",
			preferred:   "180000",
			legacy:      "30000",
			wantTimeout: 3 * time.Minute,
		},
		{
			name:        "legacy env remains supported",
			legacy:      "120000",
			wantTimeout: 2 * time.Minute,
		},
		{
			name:        "minimum clamps low values",
			preferred:   "1000",
			wantTimeout: 10 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONTAINER_IDLE_TIMEOUT_MS", tt.preferred)
			t.Setenv("IDLE_TIMEOUT_MS", tt.legacy)
			if got := containerIdleTimeoutFromEnv(); got != tt.wantTimeout {
				t.Fatalf("containerIdleTimeoutFromEnv() = %s, want %s", got, tt.wantTimeout)
			}
		})
	}
}
