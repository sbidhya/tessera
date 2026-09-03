package transport

import (
	"testing"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

func TestWireStatusDistinguishesDraw(t *testing.T) {
	tests := []struct {
		name   string
		status room.Status
		winner engine.PlayerID
		want   string
	}{
		{name: "playing", status: room.StatusPlaying, winner: engine.NoPlayer, want: "playing"},
		{name: "won", status: room.StatusFinished, winner: 0, want: "finished"},
		{name: "drawn", status: room.StatusFinished, winner: engine.NoPlayer, want: "drawn"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wireStatus(test.status, test.winner); got != test.want {
				t.Fatalf("wireStatus(%s, %d) = %q, want %q", test.status, test.winner, got, test.want)
			}
		})
	}
}
