package config

import (
	"testing"
	"time"
)

func TestRoomRetentionDefault(t *testing.T) {
	cfg := Default()
	if cfg.RoomRetention != 5*time.Minute {
		t.Errorf("RoomRetention = %s, want 5m", cfg.RoomRetention)
	}
	if DefaultRoomRetention != 5*time.Minute {
		t.Errorf("DefaultRoomRetention = %s, want 5m", cfg.RoomRetention)
	}
}

func TestLoadRoomRetentionOverride(t *testing.T) {
	cfg, err := Load(envFunc(map[string]string{"TESSERA_ROOM_RETENTION": "10m"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoomRetention != 10*time.Minute {
		t.Errorf("RoomRetention = %s, want 10m", cfg.RoomRetention)
	}
}

func TestLoadInvalidRoomRetention(t *testing.T) {
	for _, value := range []string{"soon", "0s", "-5m"} {
		if _, err := Load(envFunc(map[string]string{"TESSERA_ROOM_RETENTION": value})); err == nil {
			t.Errorf("expected error for TESSERA_ROOM_RETENTION=%q", value)
		}
	}
}
