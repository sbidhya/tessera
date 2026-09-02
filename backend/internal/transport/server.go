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
	"github.com/sbidhya/tessera/backend/internal/auth"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/match"
	"github.com/sbidhya/tessera/backend/internal/room"
)

const maxRequestBytes = 16 << 10

// Deps carries the optional B6 subsystems. A nil field disables that
// subsystem: its routes stay registered but answer 503 with a stable code, so
// the route surface is identical with or without them and clients can tell
// "not enabled" apart from "not found". A nil Auth additionally preserves the
// pre-B6 development behavior where any player_id is accepted without a token.
type Deps struct {
	Auth       *auth.Authenticator
	Matchmaker *match.Matchmaker
	Presence   *match.Presence
	// IsArchived reports whether id exists in the SQLite cold tier. It lets
	// GET /v1/matches/{id} answer 410 match_archived for an evicted match
	// instead of 404. A nil func disables the lookup (every miss is 404),
	// which is what in-memory tests without a cold tier use.
	IsArchived func(ctx context.Context, id string) (bool, error)
}

// Server owns the B3 HTTP routes and the live WebSocket hubs. The room manager
// is injected, so transport remains replaceable and persistence can wrap the
// room layer later without changing the protocol.
type Server struct {
	manager    *room.Manager
	logger     *slog.Logger
	handler    http.Handler
	auth       *auth.Authenticator
	matchmaker *match.Matchmaker
	presence   *match.Presence
	isArchived func(ctx context.Context, id string) (bool, error)

	mu     sync.Mutex
	hubs   map[string]*matchHub
	closed bool
}

func New(manager *room.Manager, logger *slog.Logger) *Server {
	return NewWithDeps(manager, logger, Deps{})
}

func NewWithDeps(manager *room.Manager, logger *slog.Logger, deps Deps) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		manager:    manager,
		logger:     logger,
		hubs:       make(map[string]*matchHub),
		auth:       deps.Auth,
		matchmaker: deps.Matchmaker,
		presence:   deps.Presence,
		isArchived: deps.IsArchived,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/matches", s.createMatch)
	mux.HandleFunc("GET /v1/matches", s.listMatches)
	mux.HandleFunc("GET /v1/matches/{matchID}", s.getState)
	mux.HandleFunc("GET /v1/matches/{matchID}/ws", s.serveWebSocket)
	mux.HandleFunc("POST /v1/players", s.createPlayer)
	mux.HandleFunc("POST /v1/matchmaking/join", s.joinMatchmaking)
	mux.HandleFunc("POST /v1/matchmaking/leave", s.leaveMatchmaking)
	mux.HandleFunc("GET /v1/matchmaking/status", s.matchmakingStatus)
	mux.HandleFunc("GET /v1/presence", s.getPresence)
	mux.HandleFunc("GET /v1/presence/{playerID}", s.getPlayerPresence)
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
	// Identity is checked before intent: a forged or missing credential fails
	// closed even when the rest of the request is well-formed.
	if !s.checkAuth(w, req.PlayerID, req.Token) {
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
		if s.isArchivedMatch(r.Context(), r.PathValue("matchID")) {
			writeError(w, http.StatusGone, "match_archived",
				"match has been archived to the cold tier; reconnect sooner after the winning move")
			return
		}
		writeError(w, http.StatusNotFound, "match_not_found", "match not found")
		return
	}
	// A private view (player_id given) requires the matching token; the
	// spectator view stays open so anyone can watch a public match.
	playerID := r.URL.Query().Get("player_id")
	if playerID != "" && !s.checkAuth(w, playerID, r.URL.Query().Get("token")) {
		return
	}
	snap, err := match.Snapshot(r.Context(), playerID)
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
	// Checked before the upgrade so a rejection still carries an HTTP status.
	if !s.checkAuth(w, playerID, r.URL.Query().Get("token")) {
		return
	}
	match, ok := s.manager.Get(r.PathValue("matchID"))
	if !ok {
		if s.isArchivedMatch(r.Context(), r.PathValue("matchID")) {
			writeError(w, http.StatusGone, "match_archived",
				"match has been archived to the cold tier; reconnect sooner after the winning move")
		} else {
			writeError(w, http.StatusNotFound, "match_not_found", "match not found")
		}
		return
	}
	// Refuse before the upgrade when the shutdown already happened, so the
	// rejection still carries an HTTP status. The hub itself is looked up
	// after the upgrade: creating it earlier would leave a fresh, empty hub
	// behind every time Accept fails.
	if s.isClosed() {
		writeError(w, http.StatusServiceUnavailable, "server_closed", "transport server is closed")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Warn("websocket accept failed", "match", match.ID(), "player", playerID, "err", err)
		return
	}
	conn.SetReadLimit(maxRequestBytes)
	client := newWSClient(playerID, conn, s.logger.With("match", match.ID(), "player", playerID))

	// A hub can retire (delete itself and exit) concurrently with this lookup:
	// hubFor never returns an exited hub, but it can return one that retires
	// immediately afterwards, in which case register reports ErrRoomClosed.
	// The hub is then already gone from the directory, so one retry with a
	// fresh hubFor carries the reconnect instead of failing it spuriously. A
	// RoomClosed from a hub that is still registered means the room itself is
	// gone, where retrying cannot help.
	var hub *matchHub
	registered := false
	for attempt := 0; attempt < 2; attempt++ {
		hub, err = s.hubFor(match)
		if err != nil {
			code := errorCode(room.ErrManagerClosed)
			writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = wsjson.Write(writeCtx, conn, Envelope{Type: "error", Payload: errorPayload{Code: code, Message: err.Error()}})
			cancel()
			client.close()
			return
		}
		if regErr := hub.register(context.Background(), client); regErr == nil {
			registered = true
			break
		} else if errors.Is(regErr, room.ErrRoomClosed) && !s.isCurrentHub(hub) {
			err = regErr
			continue
		} else {
			err = regErr
			break
		}
	}
	if !registered {
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
		// Never hand back a hub whose loop has already exited. Teardown
		// deletes under this same lock BEFORE the loop exits, so an exited
		// hub is normally already gone; a stale or terminating entry is
		// dropped here and replaced below rather than returned.
		select {
		case <-h.done:
			delete(s.hubs, match.ID())
		default:
			if h.terminating {
				delete(s.hubs, match.ID())
			} else {
				return h, nil
			}
		}
	}
	h := newMatchHub(match, s.logger, s.presence, s.removeHub)
	s.hubs[match.ID()] = h
	return h, nil
}

// removeHub deletes h from the directory and marks it terminating, all under
// s.mu — this is the hub's "I am terminating" signal back to the Server. It
// never stops the hub: the hub loop calls this and then exits itself by
// returning (which closes done). Server.Close stops hubs from outside the
// loop instead. Neither path holds s.mu across matchHub.Close, so one wedged
// hub cannot block every other connection's hubFor lookup — the same rule
// Manager.Close documents for rooms.
func (s *Server) removeHub(h *matchHub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.hubs[h.id]; ok && cur == h {
		h.terminating = true
		delete(s.hubs, h.id)
	}
}

func (s *Server) isCurrentHub(h *matchHub) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.hubs[h.id]
	return ok && cur == h
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// CloseHub stops and unregisters the hub for id. The room manager calls it
// through its evict hook so a retention eviction also retires the hub: the
// room goroutine is already gone, so any sockets still attached would only
// see ErrRoomClosed on their next op. It is a no-op when no hub is
// registered (already retired after the last disconnect, or never created).
// Like Server.Close it never holds s.mu across matchHub.Close.
func (s *Server) CloseHub(id string) {
	s.mu.Lock()
	h, ok := s.hubs[id]
	if ok {
		h.terminating = true
		delete(s.hubs, id)
	}
	s.mu.Unlock()
	if ok {
		h.Close()
	}
}

// isArchivedMatch consults the cold tier to distinguish an evicted match
// (410 match_archived) from a never-existed one (404 match_not_found). A nil
// lookup or a lookup error fails closed to "not archived" so a sick SQLite
// never turns every missing match into a 410.
func (s *Server) isArchivedMatch(ctx context.Context, id string) bool {
	if s.isArchived == nil {
		return false
	}
	ok, err := s.isArchived(ctx, id)
	if err != nil {
		s.logger.Warn("cold-tier archived lookup failed", "match", id, "err", err)
		return false
	}
	return ok
}

// checkAuth verifies the player's token when the identity layer is enabled
// and writes the rejection otherwise. In legacy mode (nil Auth) it always
// passes, preserving the pre-B6 behavior where any player_id is accepted.
func (s *Server) checkAuth(w http.ResponseWriter, playerID, token string) bool {
	if s.auth == nil {
		return true
	}
	if playerID == "" {
		writeError(w, http.StatusBadRequest, "invalid_player_id", "player_id is required")
		return false
	}
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing_token", "a token is required for this player_id")
		return false
	}
	if err := s.auth.Verify(playerID, token); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_token", "token does not match player_id")
		return false
	}
	return true
}

// createPlayer mints a fresh anonymous identity. The request body is ignored:
// identity needs no parameters, and requiring an empty JSON object would only
// add a failure mode to the first call every client makes.
func (s *Server) createPlayer(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_disabled", "server runs without the identity layer")
		return
	}
	playerID, token := s.auth.Issue()
	s.logger.Info("player identity issued", "player", playerID)
	writeJSON(w, http.StatusCreated, createPlayerResponse{PlayerID: playerID, Token: token})
}

// joinMatchmaking queues the player and blocks until a partner is found. The
// request context is the queue membership: a client that goes away is
// dequeued, and should re-queue with backoff. Because the call can stay open
// for a while, clients should set their own timeout (30s is a sane default)
// and retry — a retry while still queued attaches to the existing entry.
func (s *Server) joinMatchmaking(w http.ResponseWriter, r *http.Request) {
	if s.matchmaker == nil {
		writeError(w, http.StatusServiceUnavailable, "matchmaking_disabled", "server runs without matchmaking")
		return
	}
	var req joinMatchmakingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !s.checkAuth(w, req.PlayerID, req.Token) {
		return
	}
	result, err := s.matchmaker.Join(r.Context(), match.Request{
		PlayerID:       req.PlayerID,
		SequencesToWin: req.SequencesToWin,
	})
	if err != nil {
		// A dead request context means the client is gone; there is nobody
		// left to read an error body.
		if r.Context().Err() != nil {
			return
		}
		// The player left the queue while this long-poll was open: the wait
		// ended without a match, which is a normal outcome, not an error.
		if errors.Is(err, match.ErrLeftQueue) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeMatchmakingError(w, err)
		return
	}
	s.logger.Info("player paired", "player", req.PlayerID, "match", result.MatchID, "seat", result.Seat)
	writeJSON(w, http.StatusOK, joinMatchmakingResponse{
		MatchID:  result.MatchID,
		Seat:     int(result.Seat),
		PlayerID: req.PlayerID,
	})
}

func (s *Server) leaveMatchmaking(w http.ResponseWriter, r *http.Request) {
	if s.matchmaker == nil {
		writeError(w, http.StatusServiceUnavailable, "matchmaking_disabled", "server runs without matchmaking")
		return
	}
	var req leaveMatchmakingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !s.checkAuth(w, req.PlayerID, req.Token) {
		return
	}
	cancelled, err := s.matchmaker.Cancel(r.Context(), req.PlayerID)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeMatchmakingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, leaveMatchmakingResponse{Cancelled: cancelled})
}

func (s *Server) matchmakingStatus(w http.ResponseWriter, r *http.Request) {
	if s.matchmaker == nil {
		writeError(w, http.StatusServiceUnavailable, "matchmaking_disabled", "server runs without matchmaking")
		return
	}
	waiting, err := s.matchmaker.QueueDepth(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeMatchmakingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, matchmakingStatusResponse{Waiting: waiting})
}

func (s *Server) getPresence(w http.ResponseWriter, r *http.Request) {
	if s.presence == nil {
		writeError(w, http.StatusServiceUnavailable, "presence_disabled", "server runs without presence tracking")
		return
	}
	writeJSON(w, http.StatusOK, presenceResponse{Online: s.presence.Count()})
}

func (s *Server) getPlayerPresence(w http.ResponseWriter, r *http.Request) {
	if s.presence == nil {
		writeError(w, http.StatusServiceUnavailable, "presence_disabled", "server runs without presence tracking")
		return
	}
	playerID := r.PathValue("playerID")
	if playerID == "" {
		writeError(w, http.StatusBadRequest, "invalid_player_id", "player_id is required")
		return
	}
	writeJSON(w, http.StatusOK, playerPresenceResponse{PlayerID: playerID, Online: s.presence.IsOnline(playerID)})
}

func writeMatchmakingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, match.ErrInvalidSequencesToWin):
		writeError(w, http.StatusUnprocessableEntity, "invalid_options", err.Error())
	case errors.Is(err, room.ErrInvalidPlayerID):
		writeError(w, http.StatusBadRequest, "invalid_player_id", err.Error())
	case errors.Is(err, match.ErrMatchmakerClosed), errors.Is(err, room.ErrManagerClosed):
		writeError(w, http.StatusServiceUnavailable, "server_closed", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "matchmaking failed")
	}
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
