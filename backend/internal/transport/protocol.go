// Package transport exposes Tessera's room manager over HTTP and WebSockets.
//
// It is intentionally an outer layer: transport translates JSON into room
// commands and turns room snapshots back into JSON, but it never owns or edits
// game state. The room actor remains the sole authority for every match.
package transport

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

// Envelope is the common shape for every WebSocket message. Seq is the room
// version the message describes (server -> client) or the version a move was
// based on (client -> server).
type Envelope struct {
	Type    string `json:"type"`
	Seq     uint64 `json:"seq"`
	Payload any    `json:"payload"`
}

type inboundEnvelope struct {
	Type    string          `json:"type"`
	Seq     uint64          `json:"seq"`
	Payload json.RawMessage `json:"payload"`
}

// Card is the stable JSON representation of an engine card. Human-readable
// strings keep the wire format independent from Go enum ordinals.
type Card struct {
	Rank string `json:"rank"`
	Suit string `json:"suit"`
}

type Cell struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

type BoardCell struct {
	Cell
	Corner bool  `json:"corner"`
	Card   *Card `json:"card,omitempty"`
}

type Chip struct {
	Cell
	Owner      int  `json:"owner"`
	InSequence bool `json:"in_sequence"`
}

type Sequence struct {
	Owner int    `json:"owner"`
	Cells []Cell `json:"cells"`
}

type Player struct {
	ID      string `json:"id"`
	Seat    int    `json:"seat"`
	Present bool   `json:"present"`
}

type HandCount struct {
	Seat  int `json:"seat"`
	Count int `json:"count"`
}

type SequenceCount struct {
	Seat  int `json:"seat"`
	Count int `json:"count"`
}

// State is a per-viewer snapshot. Hand contains only the requesting player's
// cards; spectators receive an empty hand and a nil viewer.
type State struct {
	MatchID        string          `json:"match_id"`
	Status         string          `json:"status"`
	NumPlayers     int             `json:"num_players"`
	SequencesToWin int             `json:"sequences_to_win"`
	Turn           int             `json:"turn"`
	Winner         *int            `json:"winner"`
	Viewer         *int            `json:"viewer"`
	Hand           []Card          `json:"hand"`
	HandCounts     []HandCount     `json:"hand_counts"`
	Board          []BoardCell     `json:"board"`
	Chips          []Chip          `json:"chips"`
	Sequences      []Sequence      `json:"sequences"`
	SequencesWon   []SequenceCount `json:"sequences_won"`
	DrawRemaining  int             `json:"draw_remaining"`
	Players        []Player        `json:"players"`
}

type MatchSummary struct {
	ID             string `json:"id"`
	Seq            uint64 `json:"seq"`
	Status         string `json:"status"`
	Players        int    `json:"players"`
	Present        int    `json:"present"`
	Capacity       int    `json:"capacity"`
	SequencesToWin int    `json:"sequences_to_win"`
}

type createMatchRequest struct {
	SequencesToWin int `json:"sequences_to_win"`
	// PlayerID and Token authenticate the request when the server runs with
	// the B6 identity layer. They are ignored in legacy (no-auth) mode, so
	// one wire shape serves both.
	PlayerID string `json:"player_id"`
	Token    string `json:"token"`
}

// createPlayerResponse is the anonymous identity issued by POST /v1/players.
// The client must keep both halves: the id names the player, the token proves
// it. Losing the token means losing the identity — there is no recovery, by
// design (no accounts, no personal data).
type createPlayerResponse struct {
	PlayerID string `json:"player_id"`
	Token    string `json:"token"`
}

type joinMatchmakingRequest struct {
	PlayerID       string `json:"player_id"`
	Token          string `json:"token"`
	SequencesToWin int    `json:"sequences_to_win"`
}

type joinMatchmakingResponse struct {
	MatchID  string `json:"match_id"`
	Seat     int    `json:"seat"`
	PlayerID string `json:"player_id"`
}

type leaveMatchmakingRequest struct {
	PlayerID string `json:"player_id"`
	Token    string `json:"token"`
}

type leaveMatchmakingResponse struct {
	Cancelled bool `json:"cancelled"`
}

type matchmakingStatusResponse struct {
	Waiting int `json:"waiting"`
}

type presenceResponse struct {
	Online int `json:"online"`
}

type playerPresenceResponse struct {
	PlayerID string `json:"player_id"`
	Online   bool   `json:"online"`
}

type createMatchResponse struct {
	Match MatchSummary `json:"match"`
}

type listMatchesResponse struct {
	Matches []MatchSummary `json:"matches"`
}

type stateResponse struct {
	Seq   uint64 `json:"seq"`
	State State  `json:"state"`
}

type movePayload struct {
	MoveID string `json:"move_id"`
	Move   string `json:"move"`
	Card   Card   `json:"card"`
	Cell   *Cell  `json:"cell,omitempty"`
}

type moveResultPayload struct {
	MoveID    string `json:"move_id"`
	PlayerID  string `json:"player_id"`
	Duplicate bool   `json:"duplicate"`
	Status    string `json:"status"`
	Turn      int    `json:"turn"`
	Winner    *int   `json:"winner"`
}

type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorPayload `json:"error"`
}

func stateFromSnapshot(s room.Snapshot) State {
	state := State{
		MatchID:        s.RoomID,
		Status:         s.Status.String(),
		NumPlayers:     s.NumPlayers,
		SequencesToWin: s.SequencesToWin,
		Turn:           int(s.Turn),
		Hand:           make([]Card, 0, len(s.Hand)),
		HandCounts:     make([]HandCount, 0, s.NumPlayers),
		Board:          make([]BoardCell, 0, engine.BoardSize*engine.BoardSize),
		Chips:          make([]Chip, 0, len(s.Chips)),
		Sequences:      make([]Sequence, 0, len(s.Sequences)),
		SequencesWon:   make([]SequenceCount, 0, s.NumPlayers),
		DrawRemaining:  s.DrawRemaining,
		Players:        make([]Player, 0, len(s.Players)),
	}
	if s.Winner != engine.NoPlayer {
		winner := int(s.Winner)
		state.Winner = &winner
	}
	if s.Viewer != engine.NoPlayer {
		viewer := int(s.Viewer)
		state.Viewer = &viewer
	}
	for _, c := range s.Hand {
		state.Hand = append(state.Hand, cardFromEngine(c))
	}
	for seat := 0; seat < s.NumPlayers; seat++ {
		p := engine.PlayerID(seat)
		state.HandCounts = append(state.HandCounts, HandCount{Seat: seat, Count: s.HandCounts[p]})
		state.SequencesWon = append(state.SequencesWon, SequenceCount{Seat: seat, Count: s.SequencesWon[p]})
	}
	for row := 0; row < engine.BoardSize; row++ {
		for col := 0; col < engine.BoardSize; col++ {
			cell := engine.Cell{Row: row, Col: col}
			bc := BoardCell{Cell: Cell{Row: row, Col: col}, Corner: s.Board.IsCorner(cell)}
			if card, ok := s.Board.CardAt(cell); ok {
				wireCard := cardFromEngine(card)
				bc.Card = &wireCard
			}
			state.Board = append(state.Board, bc)
		}
	}
	// Row-major iteration gives clients and tests deterministic JSON ordering.
	for row := 0; row < engine.BoardSize; row++ {
		for col := 0; col < engine.BoardSize; col++ {
			cell := engine.Cell{Row: row, Col: col}
			if chip, ok := s.Chips[cell]; ok {
				state.Chips = append(state.Chips, Chip{
					Cell:       Cell{Row: row, Col: col},
					Owner:      int(chip.Owner),
					InSequence: chip.InSequence,
				})
			}
		}
	}
	for _, seq := range s.Sequences {
		cells := make([]Cell, 0, len(seq.Cells))
		for _, cell := range seq.Cells {
			cells = append(cells, Cell{Row: cell.Row, Col: cell.Col})
		}
		state.Sequences = append(state.Sequences, Sequence{Owner: int(seq.Owner), Cells: cells})
	}
	for _, p := range s.Players {
		state.Players = append(state.Players, Player{ID: p.ID, Seat: int(p.Seat), Present: p.Present})
	}
	return state
}

func summaryFromSnapshot(s room.Snapshot) MatchSummary {
	present := 0
	for _, p := range s.Players {
		if p.Present {
			present++
		}
	}
	return MatchSummary{
		ID:             s.RoomID,
		Seq:            s.Seq,
		Status:         s.Status.String(),
		Players:        len(s.Players),
		Present:        present,
		Capacity:       s.NumPlayers,
		SequencesToWin: s.SequencesToWin,
	}
}

func cardFromEngine(c engine.Card) Card {
	return Card{Rank: rankName(c.Rank), Suit: suitName(c.Suit)}
}

func (p movePayload) roomRequest(playerID string, seq uint64) (room.MoveRequest, error) {
	card, err := p.Card.engineCard()
	if err != nil {
		return room.MoveRequest{}, err
	}
	request := room.MoveRequest{
		PlayerID:    playerID,
		MoveID:      p.MoveID,
		ExpectedSeq: seq,
		Card:        card,
	}
	switch p.Move {
	case "place":
		request.Type = engine.MovePlace
	case "remove":
		request.Type = engine.MoveRemove
	case "dead_card":
		request.Type = engine.MoveDeadCard
	default:
		return room.MoveRequest{}, fmt.Errorf("unknown move %q", p.Move)
	}
	if request.Type != engine.MoveDeadCard {
		if p.Cell == nil {
			return room.MoveRequest{}, errors.New("cell is required for place and remove moves")
		}
		request.Cell = engine.Cell{Row: p.Cell.Row, Col: p.Cell.Col}
	}
	return request, nil
}

func (c Card) engineCard() (engine.Card, error) {
	rank, ok := parseRank(c.Rank)
	if !ok {
		return engine.Card{}, fmt.Errorf("unknown rank %q", c.Rank)
	}
	suit, ok := parseSuit(c.Suit)
	if !ok {
		return engine.Card{}, fmt.Errorf("unknown suit %q", c.Suit)
	}
	return engine.Card{Rank: rank, Suit: suit}, nil
}

func rankName(rank engine.Rank) string {
	switch rank {
	case engine.Ace:
		return "A"
	case engine.Two:
		return "2"
	case engine.Three:
		return "3"
	case engine.Four:
		return "4"
	case engine.Five:
		return "5"
	case engine.Six:
		return "6"
	case engine.Seven:
		return "7"
	case engine.Eight:
		return "8"
	case engine.Nine:
		return "9"
	case engine.Ten:
		return "10"
	case engine.Jack:
		return "J"
	case engine.Queen:
		return "Q"
	case engine.King:
		return "K"
	default:
		return "?"
	}
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

func suitName(suit engine.Suit) string {
	switch suit {
	case engine.Spades:
		return "spades"
	case engine.Hearts:
		return "hearts"
	case engine.Diamonds:
		return "diamonds"
	case engine.Clubs:
		return "clubs"
	default:
		return "unknown"
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

func optionalPlayer(p engine.PlayerID) *int {
	if p == engine.NoPlayer {
		return nil
	}
	v := int(p)
	return &v
}
