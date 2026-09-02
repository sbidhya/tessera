package room

import (
	"fmt"

	"github.com/sbidhya/tessera/backend/internal/engine"
)

// EventVersion is the on-disk event schema understood by this build. WAL
// records carry an explicit version so a future schema change can fail loudly
// instead of silently rebuilding the wrong game.
const EventVersion = 1

// EventType identifies one durable room state transition.
type EventType string

const (
	EventRoomCreated  EventType = "room_created"
	EventPlayerJoined EventType = "player_joined"
	EventMoveApplied  EventType = "move_applied"
	EventPlayerLeft   EventType = "player_left"
)

// Event is the persistence-neutral record emitted by the room layer. It is a
// value type on purpose: replay can compare duplicate sequence records exactly
// without depending on JSON bytes or storage implementation details.
//
// Only the fields named by Type are populated:
//   - room_created: Options and RNGSeed
//   - player_joined/player_left: PlayerID
//   - move_applied: Move
type Event struct {
	Version  int            `json:"version"`
	Type     EventType      `json:"type"`
	RoomID   string         `json:"room_id"`
	Seq      uint64         `json:"seq"`
	Options  engine.Options `json:"options,omitempty"`
	RNGSeed  [2]uint64      `json:"rng_seed,omitempty"`
	PlayerID string         `json:"player_id,omitempty"`
	Move     MoveRequest    `json:"move,omitempty"`
}

// EventJournal is the port the room layer needs from durable storage. The
// concrete filesystem WAL lives in internal/wal and depends inward on this
// package; room never imports filesystem or database code.
type EventJournal interface {
	Append(Event) error
	ReadAll() ([]Event, error)
}

// FinishedPlayer is the durable, public result for one seat. It intentionally
// omits transient presence and private hand data: the cold tier needs match
// outcomes, not a copy of connection state or hidden cards.
type FinishedPlayer struct {
	ID        string
	Seat      engine.PlayerID
	Sequences int
	Won       bool
}

// FinishedMatch is the persistence-neutral archive handed to the cold tier
// after the winning move is safely in the WAL and published in memory.
// History contains the accepted events through FinishedSeq in authoritative
// room order, allowing SQLite to serve a complete audit trail without parsing
// live room internals.
type FinishedMatch struct {
	RoomID         string
	FinishedSeq    uint64
	NumPlayers     int
	SequencesToWin int
	Winner         engine.PlayerID
	Players        []FinishedPlayer
	History        []Event
}

// FinishedMatchSink receives terminal matches for asynchronous cold storage.
// Implementations must make MatchFinished a quick enqueue operation: it runs on
// the room actor after the terminal state is committed but before its caller is
// acknowledged. The WAL remains the recovery source until the sink later
// checkpoints it, so enqueueing does not need to synchronously touch SQLite.
// archived must be called exactly once after the match is durably stored. It is
// deliberately a callback rather than a blocking result so disk I/O never runs
// on the room actor. WAL checkpointing may follow independently; the cold-tier
// record is already sufficient for eviction once archived runs.
type FinishedMatchSink interface {
	MatchFinished(match FinishedMatch, archived func())
}

func createdEvent(id string, opts engine.Options, seed [2]uint64) Event {
	return Event{
		Version: EventVersion,
		Type:    EventRoomCreated,
		RoomID:  id,
		Seq:     1,
		Options: opts,
		RNGSeed: seed,
	}
}

func (e Event) validate() error {
	if e.Version != EventVersion {
		return fmt.Errorf("room: unsupported event version %d", e.Version)
	}
	if e.RoomID == "" {
		return fmt.Errorf("room: event has empty room id")
	}
	if e.Seq == 0 {
		return fmt.Errorf("room %s: event has zero sequence", e.RoomID)
	}

	switch e.Type {
	case EventRoomCreated:
		if e.Seq != 1 {
			return fmt.Errorf("room %s: create event has sequence %d, want 1", e.RoomID, e.Seq)
		}
		if e.Options.NumPlayers == 0 {
			return fmt.Errorf("room %s: create event has no game options", e.RoomID)
		}
		if e.Options.SequencesToWin < 1 {
			return fmt.Errorf("room %s: create event has invalid sequences_to_win %d",
				e.RoomID, e.Options.SequencesToWin)
		}
	case EventPlayerJoined, EventPlayerLeft:
		if e.PlayerID == "" {
			return fmt.Errorf("room %s: %s event has empty player id", e.RoomID, e.Type)
		}
	case EventMoveApplied:
		if e.Move.PlayerID == "" || e.Move.MoveID == "" {
			return fmt.Errorf("room %s: move event has incomplete identity", e.RoomID)
		}
	default:
		return fmt.Errorf("room %s: unknown event type %q", e.RoomID, e.Type)
	}
	return nil
}
