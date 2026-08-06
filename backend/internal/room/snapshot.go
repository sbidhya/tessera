package room

import "github.com/sbidhya/tessera/backend/internal/engine"

// SeatInfo describes one chair at the table.
type SeatInfo struct {
	// Seat is the engine seat index.
	Seat engine.PlayerID
	// Player is the occupant's external id, empty if the seat is free.
	Player PlayerID
	// Occupied is true once a player has claimed the seat.
	Occupied bool
	// Present is false while the occupant is disconnected (they left mid-game
	// and the seat is being held for their return).
	Present bool
}

// Snapshot is an immutable, caller-owned view of a room at one instant. It is
// deep-copied out of the run goroutine, so holding one can never race with the
// room continuing to play.
//
// Note that it exposes every hand. That is deliberate at this layer: the room is
// the authoritative server-side view, and hiding information from ourselves buys
// nothing. Redacting opponents' hands is the transport layer's job (B3), which
// is where a per-player view belongs.
//
// The draw pile is reported only as a count. The order of undealt cards is the
// one piece of state no client may ever see, so it never leaves the room.
type Snapshot struct {
	RoomID string
	Status Status
	// Seq is the game state version this snapshot was taken at. Feed it back as
	// MoveRequest.ExpectedSeq to make a move conditional on nothing having
	// changed since.
	Seq   uint64
	Seats []SeatInfo

	NumPlayers     int
	SequencesToWin int

	// Board is shared, not copied: it is immutable after engine.NewBoard, so
	// every snapshot can safely point at the same one.
	Board *engine.Board

	Chips map[engine.Cell]engine.Chip
	Hands map[engine.PlayerID][]engine.Card

	Turn         engine.PlayerID
	SequencesWon map[engine.PlayerID]int
	Sequences    []engine.Sequence
	Winner       engine.PlayerID

	DrawCount    int
	DiscardCount int
}

// GameOver reports whether the match has a winner.
func (s Snapshot) GameOver() bool { return s.Winner != engine.NoPlayer }

// snapshot copies the room's state out. Runs on the run goroutine — it is the
// only sanctioned way for state to leave it.
func (r *Room) snapshot() Snapshot {
	g := r.game

	s := Snapshot{
		RoomID:         r.id,
		Status:         r.status,
		Seq:            r.seq,
		Seats:          make([]SeatInfo, len(r.seats)),
		NumPlayers:     g.NumPlayers,
		SequencesToWin: g.SequencesToWin,
		Board:          g.Board,
		Chips:          make(map[engine.Cell]engine.Chip, len(g.Chips)),
		Hands:          make(map[engine.PlayerID][]engine.Card, len(g.Hands)),
		Turn:           g.Turn,
		SequencesWon:   make(map[engine.PlayerID]int, len(g.SequencesWon)),
		Sequences:      make([]engine.Sequence, len(g.Sequences)),
		Winner:         g.Winner,
		DrawCount:      len(g.Draw),
		DiscardCount:   len(g.Discard),
	}

	for i := range r.seats {
		s.Seats[i] = SeatInfo{
			Seat:     engine.PlayerID(i),
			Player:   r.seats[i].player,
			Occupied: r.seats[i].occupied,
			Present:  r.seats[i].present,
		}
	}
	for cell, chip := range g.Chips {
		s.Chips[cell] = chip
	}
	for p, hand := range g.Hands {
		s.Hands[p] = append([]engine.Card(nil), hand...)
	}
	for p, n := range g.SequencesWon {
		s.SequencesWon[p] = n
	}
	copy(s.Sequences, g.Sequences)

	return s
}
