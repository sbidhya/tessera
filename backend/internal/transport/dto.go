package transport

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

// CardDTO is the wire form of an engine.Card.
// Strings are used instead of raw integers so the protocol is readable
// and stable if the internal iota values ever shift.
type CardDTO struct {
	Rank string `json:"rank"`
	Suit string `json:"suit"`
}

var rankToString = map[engine.Rank]string{
	engine.Ace: "A", engine.Two: "2", engine.Three: "3", engine.Four: "4",
	engine.Five: "5", engine.Six: "6", engine.Seven: "7", engine.Eight: "8",
	engine.Nine: "9", engine.Ten: "10", engine.Jack: "J", engine.Queen: "Q", engine.King: "K",
}

var stringToRank = map[string]engine.Rank{
	"A": engine.Ace, "2": engine.Two, "3": engine.Three, "4": engine.Four,
	"5": engine.Five, "6": engine.Six, "7": engine.Seven, "8": engine.Eight,
	"9": engine.Nine, "10": engine.Ten, "J": engine.Jack, "Q": engine.Queen, "K": engine.King,
	// Accept long names as well for ergonomics.
	"Ace": engine.Ace, "Jack": engine.Jack, "Queen": engine.Queen, "King": engine.King,
}

var suitToString = map[engine.Suit]string{
	engine.Spades: "Spades", engine.Hearts: "Hearts", engine.Diamonds: "Diamonds", engine.Clubs: "Clubs",
}

var stringToSuit = map[string]engine.Suit{
	"Spades":   engine.Spades,
	"Hearts":   engine.Hearts,
	"Diamonds": engine.Diamonds,
	"Clubs":    engine.Clubs,
	// Short forms / symbols tolerated on ingress.
	"S": engine.Spades, "H": engine.Hearts, "D": engine.Diamonds, "C": engine.Clubs,
	"♠": engine.Spades, "♥": engine.Hearts, "♦": engine.Diamonds, "♣": engine.Clubs,
}

func cardToDTO(c engine.Card) CardDTO {
	return CardDTO{Rank: rankToString[c.Rank], Suit: suitToString[c.Suit]}
}

func (d CardDTO) toCard() (engine.Card, error) {
	r, ok := stringToRank[d.Rank]
	if !ok {
		return engine.Card{}, fmt.Errorf("invalid rank %q", d.Rank)
	}
	s, ok := stringToSuit[d.Suit]
	if !ok {
		return engine.Card{}, fmt.Errorf("invalid suit %q", d.Suit)
	}
	return engine.Card{Rank: r, Suit: s}, nil
}

// CellDTO is the wire form of engine.Cell.
type CellDTO struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

func cellToDTO(c engine.Cell) CellDTO { return CellDTO{Row: c.Row, Col: c.Col} }
func (d CellDTO) toCell() engine.Cell { return engine.Cell{Row: d.Row, Col: d.Col} }

// ChipDTO is the wire form of engine.Chip.
type ChipDTO struct {
	Owner      int  `json:"owner"`
	InSequence bool `json:"in_sequence"`
}

// SequenceDTO is the wire form of engine.Sequence.
type SequenceDTO struct {
	Owner int       `json:"owner"`
	Cells []CellDTO `json:"cells"`
}

// PlayerInfoDTO is the wire form of room.PlayerInfo.
type PlayerInfoDTO struct {
	ID      string `json:"id"`
	Seat    int    `json:"seat"`
	Present bool   `json:"present"`
}

// SnapshotDTO is the JSON shape returned by GET /matches/{id} and broadcast
// over WS as a "state" envelope. It is per-viewer: Hand is only the viewer's
// own cards.
type SnapshotDTO struct {
	RoomID        string             `json:"room_id"`
	Seq           uint64             `json:"seq"`
	Status        string             `json:"status"`
	Turn          int                `json:"turn"`
	Winner        int                `json:"winner"`
	Viewer        int                `json:"viewer"`
	Hand          []CardDTO          `json:"hand,omitempty"`
	HandCounts    map[string]int     `json:"hand_counts"`
	Chips         map[string]ChipDTO `json:"chips"`
	Sequences     []SequenceDTO      `json:"sequences"`
	SequencesWon  map[string]int     `json:"sequences_won"`
	DrawRemaining int                `json:"draw_remaining"`
	Players       []PlayerInfoDTO    `json:"players"`
	// Board is a flat list of non-corner cells for clients that need
	// the card printed on each cell (e.g. mobile board rendering).
	Board []BoardCellDTO `json:"board,omitempty"`
}

type BoardCellDTO struct {
	Cell CellDTO `json:"cell"`
	Card CardDTO `json:"card"`
}

func snapshotToDTO(s room.Snapshot) SnapshotDTO {
	dto := SnapshotDTO{
		RoomID:        s.RoomID,
		Seq:           s.Seq,
		Status:        s.Status.String(),
		Turn:          int(s.Turn),
		Winner:        int(s.Winner),
		Viewer:        int(s.Viewer),
		HandCounts:    make(map[string]int, len(s.HandCounts)),
		Chips:         make(map[string]ChipDTO, len(s.Chips)),
		SequencesWon:  make(map[string]int, len(s.SequencesWon)),
		DrawRemaining: s.DrawRemaining,
	}
	for seat, n := range s.HandCounts {
		dto.HandCounts[fmt.Sprintf("%d", int(seat))] = n
	}
	for seat, n := range s.SequencesWon {
		dto.SequencesWon[fmt.Sprintf("%d", int(seat))] = n
	}
	for c, ch := range s.Chips {
		k := fmt.Sprintf("%d,%d", c.Row, c.Col)
		dto.Chips[k] = ChipDTO{Owner: int(ch.Owner), InSequence: ch.InSequence}
	}
	dto.Sequences = make([]SequenceDTO, 0, len(s.Sequences))
	for _, seq := range s.Sequences {
		cells := make([]CellDTO, len(seq.Cells))
		for i, c := range seq.Cells {
			cells[i] = cellToDTO(c)
		}
		dto.Sequences = append(dto.Sequences, SequenceDTO{Owner: int(seq.Owner), Cells: cells})
	}
	// Deterministic order: sort by owner then lexicographically by first cell.
	sort.Slice(dto.Sequences, func(i, j int) bool {
		if dto.Sequences[i].Owner != dto.Sequences[j].Owner {
			return dto.Sequences[i].Owner < dto.Sequences[j].Owner
		}
		if len(dto.Sequences[i].Cells) == 0 || len(dto.Sequences[j].Cells) == 0 {
			return len(dto.Sequences[i].Cells) < len(dto.Sequences[j].Cells)
		}
		a, b := dto.Sequences[i].Cells[0], dto.Sequences[j].Cells[0]
		if a.Row != b.Row {
			return a.Row < b.Row
		}
		return a.Col < b.Col
	})
	dto.Hand = make([]CardDTO, 0, len(s.Hand))
	for _, c := range s.Hand {
		dto.Hand = append(dto.Hand, cardToDTO(c))
	}
	dto.Players = make([]PlayerInfoDTO, 0, len(s.Players))
	for _, p := range s.Players {
		dto.Players = append(dto.Players, PlayerInfoDTO{ID: p.ID, Seat: int(p.Seat), Present: p.Present})
	}
	sort.Slice(dto.Players, func(i, j int) bool { return dto.Players[i].Seat < dto.Players[j].Seat })
	// Board mapping (exclude corners). Shared board pointer is immutable.
	if s.Board != nil {
		for r := 0; r < engine.BoardSize; r++ {
			for c := 0; c < engine.BoardSize; c++ {
				cell := engine.Cell{Row: r, Col: c}
				if s.Board.IsCorner(cell) {
					continue
				}
				card, ok := s.Board.CardAt(cell)
				if !ok {
					continue
				}
				dto.Board = append(dto.Board, BoardCellDTO{Cell: cellToDTO(cell), Card: cardToDTO(card)})
			}
		}
		// Already row-major order from the loops, stable.
	}
	return dto
}

// CreateMatchRequest is the body for POST /matches.
type CreateMatchRequest struct {
	NumPlayers     *int `json:"num_players,omitempty"`
	SequencesToWin *int `json:"sequences_to_win,omitempty"`
}

// CreateMatchResponse is the success payload for POST /matches.
type CreateMatchResponse struct {
	RoomID string `json:"room_id"`
	Seq    uint64 `json:"seq"`
	Status string `json:"status"`
}

// ListMatchesResponse is the payload for GET /matches.
type ListMatchesResponse struct {
	Rooms []RoomSummaryDTO `json:"rooms"`
}

type RoomSummaryDTO struct {
	RoomID  string          `json:"room_id"`
	Seq     uint64          `json:"seq"`
	Status  string          `json:"status"`
	Players []PlayerInfoDTO `json:"players"`
}

// JoinRequest is the body for POST /matches/{id}/join.
type JoinRequest struct {
	PlayerID string `json:"player_id"`
}

// JoinResponse mirrors room.JoinResult for JSON.
type JoinResponse struct {
	Seat     int    `json:"seat"`
	Rejoined bool   `json:"rejoined"`
	Seq      uint64 `json:"seq"`
	Status   string `json:"status"`
}

// MovePayload is the payload of a WS {type:"move"} envelope.
type MovePayload struct {
	MoveID      string   `json:"move_id"`
	ExpectedSeq uint64   `json:"expected_seq,omitempty"`
	Type        string   `json:"type"` // "place" | "remove" | "dead_card"
	Card        CardDTO  `json:"card"`
	Cell        *CellDTO `json:"cell,omitempty"`
}

// MoveResultDTO is the payload for a successful move ack/broadcast.
type MoveResultDTO struct {
	Seq       uint64 `json:"seq"`
	Duplicate bool   `json:"duplicate"`
	Status    string `json:"status"`
	Turn      int    `json:"turn"`
	Winner    int    `json:"winner"`
}

func moveResultToDTO(r room.MoveResult) MoveResultDTO {
	return MoveResultDTO{
		Seq:       r.Seq,
		Duplicate: r.Duplicate,
		Status:    r.Status.String(),
		Turn:      int(r.Turn),
		Winner:    int(r.Winner),
	}
}

// ErrorDTO is the payload for {type:"error"} envelopes and for HTTP error bodies.
type ErrorDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func errorDTO(code, msg string) ErrorDTO { return ErrorDTO{Code: code, Message: msg} }

// Parse MovePayload.Type to engine.MoveType.
func (m MovePayload) toMoveType() (engine.MoveType, error) {
	switch m.Type {
	case "place":
		return engine.MovePlace, nil
	case "remove":
		return engine.MoveRemove, nil
	case "dead_card", "dead", "deadcard":
		return engine.MoveDeadCard, nil
	default:
		return 0, fmt.Errorf("unknown move type %q", m.Type)
	}
}

// json helper to avoid importing encoding/json in callers that only need the DTO.
var _ = json.Marshal
