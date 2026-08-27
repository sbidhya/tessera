package wal

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

// Replay rebuilds all matches found in the WAL directory into the given
// manager. It is idempotent: replaying the same files twice produces the same
// final state (moves are deduped via move_id, joins via idempotency).
//
// The manager must have been created with the same seed/randFunc as the
// original process so that board layouts and deals are byte-identical. Rooms
// are created with no WAL during replay, so no new log entries are produced;
// after replay the manager and every room are wired to this store for future
// writes.
func (s *Store) Replay(manager *room.Manager) error {
	if s == nil || manager == nil {
		return nil
	}
	ids, err := s.ListRooms()
	if err != nil {
		return fmt.Errorf("wal: list rooms: %w", err)
	}
	sort.Strings(ids)

	for _, id := range ids {
		// Heal a torn tail before reading.
		if err := s.TruncateToValid(id); err != nil {
			// Log but continue; a single bad file should not block others.
			slog.Default().Warn("wal: truncate failed", "match", id, "err", err)
		}
		records, err := s.ReadRecords(id)
		if err != nil {
			return fmt.Errorf("wal: read %s: %w", id, err)
		}
		if len(records) == 0 {
			continue
		}
		if records[0].Type != "create" || records[0].Options == nil {
			// No valid create — cannot reconstruct. Skip.
			slog.Default().Warn("wal: missing create record", "match", id)
			continue
		}
		opts := engine.Options{
			NumPlayers:     records[0].Options.NumPlayers,
			SequencesToWin: records[0].Options.SequencesToWin,
		}
		// Idempotent replay: if the room already exists (duplicate replay
		// onto the same manager), reuse it instead of failing.
		var r *room.Room
		if existing, ok := manager.Get(id); ok {
			r = existing
			// If the existing room was already built from a previous replay,
			// its state already reflects earlier records. Re-applying the same
			// records must be idempotent (moves deduped via move_id).
		} else {
			var err error
			r, err = manager.Restore(id, opts)
			if err != nil {
				return fmt.Errorf("wal: restore room %s: %w", id, err)
			}
		}
		// Replay remaining records in log order. When the room was reused
		// (duplicate replay), re-applying is idempotent via move_id and
		// join idempotency.
		for _, rec := range records[1:] {
			switch rec.Type {
			case "join":
				if rec.PlayerID == "" {
					continue
				}
				_, _ = r.Join(context.Background(), rec.PlayerID)
			case "leave":
				if rec.PlayerID == "" {
					continue
				}
				_ = r.Leave(context.Background(), rec.PlayerID)
			case "move":
				req, err := recordToMoveRequest(rec)
				if err != nil {
					slog.Default().Warn("wal: skip bad move record", "match", id, "err", err)
					continue
				}
				_, _ = r.PlayMove(context.Background(), req)
			default:
				slog.Default().Warn("wal: unknown record type", "match", id, "type", rec.Type)
			}
		}
	}
	// Wire the store for future writes.
	manager.SetWAL(s)
	return nil
}

func recordToMoveRequest(rec Record) (room.MoveRequest, error) {
	if rec.MoveID == "" {
		return room.MoveRequest{}, fmt.Errorf("missing move_id")
	}
	var mt engine.MoveType
	switch rec.MoveType {
	case "place":
		mt = engine.MovePlace
	case "remove":
		mt = engine.MoveRemove
	case "dead_card":
		mt = engine.MoveDeadCard
	default:
		return room.MoveRequest{}, fmt.Errorf("unknown move type %q", rec.MoveType)
	}
	var card engine.Card
	if rec.Card != nil {
		var err error
		card, err = parseCard(*rec.Card)
		if err != nil {
			return room.MoveRequest{}, err
		}
	}
	cell := engine.Cell{}
	if rec.Cell != nil {
		cell = engine.Cell{Row: rec.Cell.Row, Col: rec.Cell.Col}
	}
	return room.MoveRequest{
		PlayerID:    rec.PlayerID,
		MoveID:      rec.MoveID,
		ExpectedSeq: rec.ExpectedSeq,
		Type:        mt,
		Card:        card,
		Cell:        cell,
	}, nil
}

func parseCard(c Card) (engine.Card, error) {
	r, ok := parseRank(c.Rank)
	if !ok {
		return engine.Card{}, fmt.Errorf("unknown rank %q", c.Rank)
	}
	s, ok := parseSuit(c.Suit)
	if !ok {
		return engine.Card{}, fmt.Errorf("unknown suit %q", c.Suit)
	}
	return engine.Card{Rank: r, Suit: s}, nil
}

func parseRank(rank string) (engine.Rank, bool) {
	switch rank {
	case "A":
		return engine.Ace, true
	case "2":
		return engine.Two, true
	case "3":
		return engine.Three, true
	case "4":
		return engine.Four, true
	case "5":
		return engine.Five, true
	case "6":
		return engine.Six, true
	case "7":
		return engine.Seven, true
	case "8":
		return engine.Eight, true
	case "9":
		return engine.Nine, true
	case "10":
		return engine.Ten, true
	case "J":
		return engine.Jack, true
	case "Q":
		return engine.Queen, true
	case "K":
		return engine.King, true
	default:
		return 0, false
	}
}

func parseSuit(suit string) (engine.Suit, bool) {
	switch suit {
	case "spades":
		return engine.Spades, true
	case "hearts":
		return engine.Hearts, true
	case "diamonds":
		return engine.Diamonds, true
	case "clubs":
		return engine.Clubs, true
	default:
		return 0, false
	}
}
