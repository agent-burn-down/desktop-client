package main

import (
	"strconv"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
)

func TestResolvePort(t *testing.T) {
	tests := []struct {
		name          string
		explicitFlag  bool
		flagPort      int
		persistedPort int // 0 means no config saved
		want          int
	}{
		{
			name:          "explicit flag wins over persisted",
			explicitFlag:  true,
			flagPort:      9000,
			persistedPort: 8766,
			want:          9000,
		},
		{
			name:          "persisted port used when flag not set",
			flagPort:      receiver.DefaultPort,
			persistedPort: 8766,
			want:          8766,
		},
		{
			name:     "falls back to default when nothing persisted",
			flagPort: receiver.DefaultPort,
			want:     receiver.DefaultPort,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvConfigDir, t.TempDir())
			if tc.persistedPort != 0 {
				store, err := config.NewFileStore()
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Save(&config.Config{ReceiverPort: tc.persistedPort}); err != nil {
					t.Fatal(err)
				}
			}

			cmd := newDoctorCmd()
			if tc.explicitFlag {
				if err := cmd.Flags().Set("port", strconv.Itoa(tc.flagPort)); err != nil {
					t.Fatal(err)
				}
			}
			if got := resolvePort(cmd, tc.flagPort); got != tc.want {
				t.Errorf("resolvePort() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResolvePortMissingConfigFallsBackToFlag(t *testing.T) {
	// No config saved: Load fails, and resolvePort must fall back to
	// flagPort rather than erroring.
	t.Setenv(config.EnvConfigDir, t.TempDir())
	cmd := newStatusCmd()
	if got := resolvePort(cmd, receiver.DefaultPort); got != receiver.DefaultPort {
		t.Errorf("resolvePort() = %d, want default %d", got, receiver.DefaultPort)
	}
}
