package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

const maxRequestBytes = 16 << 10

// Server owns the B3 HTTP routes and the live WebSocket hubs. The room manager
// is injected, so transport remains replaceable and persistence can wrap the
// room layer later without changing the protocol.
type Server struct {
	manager *room.Manager
	logger  *slog.Logger
	handler http.Handler

	mu     sync.Mutex
	hubs   map[string]*matchHub
	closed bool
}

func New(manager *room.Manager, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{manager: manager, logger: logger, hubs: make(map[string]*matchHub)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matches", s.createMatch)
	mux.HandleFunc("GET /v1/matches", s.listMatches)
	mux.HandleFunc("GET /v1/matches/{matchID}", s.getState)
	mux.HandleFunc("GET /v1/matches/{matchID}/ws", s.serveWebSocket)
	s.handler = mux
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

// Close disconnects WebSocket clients and stops every hub. Room lifetime stays
// with room.Manager, which the process shuts down immediately afterwards.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	hubs := make([]*matchHub, 0, len(s.hubs))
	for _, h := range s.hubs {
		hubs = append(hubs, h)
	}
	s.hubs = make(map[string]*matchHub)
	s.mu.Unlock()

	for _, h := range hubs {
		h.Close()
	}
}

func (s *Server) createMatch(w http.ResponseWriter, r *http.Request) {
	var req createMatchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.SequencesToWin == 0 {
		req.SequencesToWin = 2
	}
	if req.SequencesToWin < 0 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_options", "sequences_to_win must be positive")
		return
	}
	match, err := s.manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: req.SequencesToWin})
	if err != nil {
		status, code := httpError(err)
		writeError(w, status, code, err.Error())
		return
	}
	snap, err := match.Snapshot(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not read created match")
		return
	}
	writeJSON(w, http.StatusCreated, createMatchResponse{Match: summaryFromSnapshot(snap)})
}

func (s *Server) listMatches(w http.ResponseWriter, r *http.Request) {
	matches := make([]MatchSummary, 0)
	for _, match := range s.manager.List() {
		snap, err := match.Snapshot(r.Context(), "")
		if err != nil {
			if errors.Is(err, room.ErrRoomClosed) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "could not list matches")
			return
		}
		matches = append(matches, summaryFromSnapshot(snap))
	}
	writeJSON(w, http.StatusOK, listMatchesResponse{Matches: matches})
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	match, ok := s.manager.Get(r.PathValue("matchID"))
	if !ok {
		writeError(w, http.StatusNotFound, "match_not_found", "match not found")
		return
	}
	snap, err := match.Snapshot(r.Context(), r.URL.Query().Get("player_id"))
	if err != nil {
		status, code := httpError(err)
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{Seq: snap.Seq, State: stateFromSnapshot(snap)})
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	playerID := r.URL.Query().Get("player_id")
	if playerID == "" {
		writeError(w, http.StatusBadRequest, "invalid_player_id", "player_id is required")
		return
	}
	match, ok := s.manager.Get(r.PathValue("matchID"))
	if !ok {
		writeError(w, http.StatusNotFound, "match_not_found", "match not found")
		return
	}
	hub, err := s.hubFor(match)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_closed", err.Error())
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Warn("websocket accept failed", "match", match.ID(), "player", playerID, "err", err)
		return
	}
	conn.SetReadLimit(maxRequestBytes)
	client := newWSClient(playerID, conn, s.logger.With("match", match.ID(), "player", playerID))

	if err := hub.register(context.Background(), client); err != nil {
		code := errorCode(err)
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = wsjson.Write(writeCtx, conn, Envelope{Type: "error", Payload: errorPayload{Code: code, Message: err.Error()}})
		cancel()
		client.close()
		return
	}
	go client.writeLoop()
	defer func() {
		hub.unregister(context.Background(), client)
		client.close()
	}()

	for {
		var envelope inboundEnvelope
		if err := wsjson.Read(r.Context(), conn, &envelope); err != nil {
			return
		}
		if envelope.Type != "move" {
			s.sendClientError(client, hub.currentSeq(), "unsupported_message",
				fmt.Sprintf("unsupported message type %q", envelope.Type))
			continue
		}

		var payload movePayload
		if err := decodeRawJSON(envelope.Payload, &payload); err != nil {
			s.sendClientError(client, hub.currentSeq(), "invalid_payload", err.Error())
			continue
		}
		request, err := payload.roomRequest(playerID, envelope.Seq)
		if err != nil {
			s.sendClientError(client, hub.currentSeq(), "invalid_payload", err.Error())
			continue
		}
		outcome := hub.move(r.Context(), client, request)
		if outcome.err != nil {
			s.sendClientError(client, outcome.seq, errorCode(outcome.err), outcome.err.Error())
		}
	}
}

func (s *Server) sendClientError(client *wsClient, seq uint64, code, message string) {
	if !client.enqueue(Envelope{
		Type:    "error",
		Seq:     seq,
		Payload: errorPayload{Code: code, Message: message},
	}) {
		client.close()
	}
}

func (s *Server) hubFor(match *room.Room) (*matchHub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("transport server is closed")
	}
	if h, ok := s.hubs[match.ID()]; ok {
		return h, nil
	}
	h := newMatchHub(match, s.logger)
	s.hubs[match.ID()] = h
	return h, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

func decodeRawJSON(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("payload is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("payload must contain one JSON object")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorPayload{Code: code, Message: message}})
}

func httpError(err error) (int, string) {
	switch {
	case errors.Is(err, room.ErrNoSuchRoom):
		return http.StatusNotFound, "match_not_found"
	case errors.Is(err, room.ErrManagerClosed), errors.Is(err, room.ErrRoomClosed):
		return http.StatusServiceUnavailable, "server_closed"
	case errors.Is(err, room.ErrDurability):
		return http.StatusServiceUnavailable, "durability_failure"
	default:
		return http.StatusUnprocessableEntity, errorCode(err)
	}
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, room.ErrRoomFull):
		return "room_full"
	case errors.Is(err, room.ErrNotSeated):
		return "not_seated"
	case errors.Is(err, room.ErrGameNotStarted):
		return "game_not_started"
	case errors.Is(err, room.ErrGameFinished), errors.Is(err, engine.ErrGameOver):
		return "game_finished"
	case errors.Is(err, room.ErrInvalidPlayerID):
		return "invalid_player_id"
	case errors.Is(err, room.ErrMissingMoveID):
		return "missing_move_id"
	case errors.Is(err, room.ErrStaleSeq):
		return "stale_seq"
	case errors.Is(err, room.ErrRoomClosed), errors.Is(err, room.ErrManagerClosed):
		return "server_closed"
	case errors.Is(err, room.ErrDurability):
		return "durability_failure"
	case errors.Is(err, engine.ErrNotYourTurn):
		return "not_your_turn"
	case errors.Is(err, engine.ErrUnknownPlayer):
		return "unknown_player"
	case errors.Is(err, engine.ErrCardNotInHand):
		return "card_not_in_hand"
	case errors.Is(err, engine.ErrCellOutOfBounds):
		return "cell_out_of_bounds"
	case errors.Is(err, engine.ErrCellOccupied):
		return "cell_occupied"
	case errors.Is(err, engine.ErrCellIsCorner):
		return "cell_is_corner"
	case errors.Is(err, engine.ErrCardCellMismatch):
		return "card_cell_mismatch"
	case errors.Is(err, engine.ErrNotRemovable):
		return "not_removable"
	case errors.Is(err, engine.ErrNotOneEyedJack):
		return "not_one_eyed_jack"
	case errors.Is(err, engine.ErrCardNotDead):
		return "card_not_dead"
	case errors.Is(err, engine.ErrDeadCardUsed):
		return "dead_card_already_used"
	case errors.Is(err, engine.ErrJackNotPlaceable):
		return "jack_not_placeable"
	default:
		return "internal_error"
	}
}
