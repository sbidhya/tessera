package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

// Server is the HTTP + WebSocket surface for the game.
// It owns no game state; it delegates to room.Manager and translates
// HTTP/WS into room commands. The layered diagram is still
// engine <- room <- transport: transport imports room+engine, never the reverse.
type Server struct {
	manager *room.Manager
	logger  *slog.Logger
	start   time.Time
	now     func() time.Time
	hubs    *hubRegistry
	mux     *http.ServeMux
	handler http.Handler
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// In dev the client may run on a different origin (Flutter web, emulator).
	// Real auth arrives in B6; until then allowing any origin is the pragmatic
	// default. The upgrader still checks the WebSocket handshake itself.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// New builds a Server that serves on top of mgr.
// start/now mirror cmd/tessera's healthz uptime plumbing; pass time.Now for now.
func New(mgr *room.Manager, logger *slog.Logger, start time.Time, now func() time.Time) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if mgr == nil {
		panic("transport: nil manager")
	}
	s := &Server{
		manager: mgr,
		logger:  logger,
		start:   start,
		now:     now,
		hubs:    newHubRegistry(),
		mux:     http.NewServeMux(),
	}
	s.routes()
	// Request logging wraps the whole mux so even 404s are logged.
	s.handler = requestLogger(logger, s.mux)
	return s
}

// Handler returns the http.Handler to serve (including logging wrapper).
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() {
	// Health.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)

	// REST — support both /api/matches and /matches so Flutter's M2 can use
	// the conventional /api prefix while curl examples stay short.
	s.mux.HandleFunc("POST /api/matches", s.handleCreate)
	s.mux.HandleFunc("POST /matches", s.handleCreate)

	s.mux.HandleFunc("GET /api/matches", s.handleList)
	s.mux.HandleFunc("GET /matches", s.handleList)

	s.mux.HandleFunc("GET /api/matches/{id}", s.handleGet)
	s.mux.HandleFunc("GET /matches/{id}", s.handleGet)

	s.mux.HandleFunc("POST /api/matches/{id}/join", s.handleJoin)
	s.mux.HandleFunc("POST /matches/{id}/join", s.handleJoin)

	// WebSocket.
	s.mux.HandleFunc("GET /api/matches/{id}/ws", s.handleWS)
	s.mux.HandleFunc("GET /matches/{id}/ws", s.handleWS)
}

// ---- REST handlers ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"status": "ok",
		"uptime": s.now().Sub(s.start).Round(time.Millisecond).String(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateMatchRequest
	// Body is optional; an empty body means defaults (2 players, 2 to win).
	if r.ContentLength != 0 {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
				return
			}
		}
	}
	numPlayers := 2
	if req.NumPlayers != nil {
		numPlayers = *req.NumPlayers
	}
	seqToWin := 2
	if req.SequencesToWin != nil {
		seqToWin = *req.SequencesToWin
	}
	opts := engine.Options{NumPlayers: numPlayers, SequencesToWin: seqToWin}
	rm, err := s.manager.Create(opts)
	if err != nil {
		// Currently only engine validation errors.
		writeError(w, http.StatusBadRequest, "invalid_options", err.Error())
		return
	}
	// Snapshot to get seq/status for the response.
	snap, _ := rm.Snapshot(context.Background(), "")
	resp := CreateMatchResponse{RoomID: rm.ID(), Seq: snap.Seq, Status: snap.Status.String()}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	rooms := s.manager.List()
	summaries := make([]RoomSummaryDTO, 0, len(rooms))
	for _, rm := range rooms {
		snap, _ := rm.Snapshot(context.Background(), "")
		// Convert players to DTO for list as well.
		var players []PlayerInfoDTO
		for _, p := range snap.Players {
			players = append(players, PlayerInfoDTO{ID: p.ID, Seat: int(p.Seat), Present: p.Present})
		}
		if players == nil {
			players = []PlayerInfoDTO{}
		}
		summaries = append(summaries, RoomSummaryDTO{RoomID: rm.ID(), Seq: snap.Seq, Status: snap.Status.String(), Players: players})
	}
	writeJSON(w, http.StatusOK, ListMatchesResponse{Rooms: summaries})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing match id")
		return
	}
	rm, ok := s.manager.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no_such_room", "no such room "+id)
		return
	}
	viewer := r.URL.Query().Get("player_id")
	// Also accept ?viewer= for ergonomics.
	if viewer == "" {
		viewer = r.URL.Query().Get("viewer")
	}
	snap, err := rm.Snapshot(r.Context(), viewer)
	if err != nil {
		// Snapshot only fails on closed room or cancelled ctx.
		if errors.Is(err, room.ErrRoomClosed) {
			writeError(w, http.StatusGone, "room_closed", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshotToDTO(snap))
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing match id")
		return
	}
	rm, ok := s.manager.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no_such_room", "no such room "+id)
		return
	}
	var req JoinRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
			return
		}
	}
	// Also accept ?player_id= for curl ergonomics.
	if req.PlayerID == "" {
		req.PlayerID = r.URL.Query().Get("player_id")
		if req.PlayerID == "" {
			req.PlayerID = r.URL.Query().Get("viewer")
		}
	}
	if req.PlayerID == "" {
		writeError(w, http.StatusBadRequest, "missing_player_id", "player_id is required")
		return
	}
	res, err := rm.Join(r.Context(), req.PlayerID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	// Notify WS observers that roster/seq changed.
	s.broadcastState(rm)
	writeJSON(w, http.StatusOK, JoinResponse{Seat: int(res.Seat), Rejoined: res.Rejoined, Seq: res.Seq, Status: res.Status.String()})
}

// ---- WebSocket ----

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("id")
	if roomID == "" {
		http.Error(w, "missing match id", http.StatusBadRequest)
		return
	}
	playerID := r.URL.Query().Get("player_id")
	if playerID == "" {
		playerID = r.URL.Query().Get("viewer")
	}
	if playerID == "" {
		http.Error(w, "player_id query param is required", http.StatusBadRequest)
		return
	}
	rm, ok := s.manager.Get(roomID)
	if !ok {
		http.Error(w, "no such room", http.StatusNotFound)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error.
		s.logger.Debug("ws upgrade failed", "room", roomID, "err", err)
		return
	}
	// Ensure we close the underlying connection when the handler returns.
	defer conn.Close()

	// Join is idempotent; a reconnecting player re-occupies their seat.
	joinRes, err := rm.Join(r.Context(), playerID)
	if err != nil {
		// Report join failure over the already-upgraded socket, then close.
		_ = conn.WriteJSON(Envelope{Type: "error", Seq: 0, Payload: mustMarshal(errorDTO(mapErrorCode(err), err.Error()))})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error()))
		return
	}
	_ = joinRes // seq available for initial payload, but Snapshot is canonical.

	client := &wsClient{
		conn:     conn,
		send:     make(chan Envelope, 32),
		roomID:   roomID,
		playerID: playerID,
		logger:   s.logger.With("room", roomID, "player", playerID),
	}
	s.hubs.add(client)
	defer s.hubs.remove(client)

	// Writer goroutine.
	done := make(chan struct{})
	go client.writeLoop(done)
	defer func() { close(done) }()

	// Notify existing peers that someone joined (status may have flipped to playing).
	s.broadcastState(rm)

	// Send initial state to this connection. Using the broadcast would also reach
	// this client, but sending one direct snapshot guarantees the newcomer gets
	// state even if broadcast races with its own add.
	if snap, err := rm.Snapshot(context.Background(), playerID); err == nil {
		dto := snapshotToDTO(snap)
		env := Envelope{Type: "state", Seq: snap.Seq, Payload: mustMarshal(dto)}
		select {
		case client.send <- env:
		default:
		}
	}

	// Set reasonable limits; mobile clients may be on flaky networks.
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Time{})
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		var env Envelope
		if err := conn.ReadJSON(&env); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				s.logger.Debug("ws read error", "room", roomID, "player", playerID, "err", err)
			}
			break
		}
		switch env.Type {
		case "move":
			s.handleWSMove(rm, client, env)
		case "ping":
			// Echo as pong so a client can measure RTT.
			snap, _ := rm.Snapshot(context.Background(), playerID)
			seq := uint64(0)
			if snap.Seq != 0 {
				seq = snap.Seq
			}
			_ = snap
			select {
			case client.send <- Envelope{Type: "pong", Seq: seq}:
			default:
			}
		default:
			// Unknown type: tell this client only.
			snap, _ := rm.Snapshot(context.Background(), playerID)
			seq := uint64(0)
			if snap.Seq != 0 {
				seq = snap.Seq
			}
			s.sendError(client, seq, "unknown_type", "unknown envelope type "+strconv.Quote(env.Type))
		}
	}
}

func (s *Server) handleWSMove(rm *room.Room, client *wsClient, env Envelope) {
	var mp MovePayload
	if err := json.Unmarshal(env.Payload, &mp); err != nil {
		snap, _ := rm.Snapshot(context.Background(), client.playerID)
		seq := uint64(0)
		if snap.Seq != 0 {
			seq = snap.Seq
		}
		s.sendError(client, seq, "bad_payload", "invalid move payload: "+err.Error())
		return
	}
	if mp.MoveID == "" {
		snap, _ := rm.Snapshot(context.Background(), client.playerID)
		seq := uint64(0)
		if snap.Seq != 0 {
			seq = snap.Seq
		}
		s.sendError(client, seq, "missing_move_id", "move_id is required")
		return
	}
	mtype, err := mp.toMoveType()
	if err != nil {
		snap, _ := rm.Snapshot(context.Background(), client.playerID)
		seq := uint64(0)
		if snap.Seq != 0 {
			seq = snap.Seq
		}
		s.sendError(client, seq, "bad_move_type", err.Error())
		return
	}
	card, err := mp.Card.toCard()
	if err != nil {
		snap, _ := rm.Snapshot(context.Background(), client.playerID)
		seq := uint64(0)
		if snap.Seq != 0 {
			seq = snap.Seq
		}
		s.sendError(client, seq, "bad_card", err.Error())
		return
	}
	cell := engine.Cell{}
	if mtype != engine.MoveDeadCard {
		if mp.Cell == nil {
			snap, _ := rm.Snapshot(context.Background(), client.playerID)
			seq := uint64(0)
			if snap.Seq != 0 {
				seq = snap.Seq
			}
			s.sendError(client, seq, "missing_cell", "cell is required for place/remove")
			return
		}
		cell = mp.Cell.toCell()
	}

	req := room.MoveRequest{
		PlayerID:    client.playerID,
		MoveID:      mp.MoveID,
		ExpectedSeq: mp.ExpectedSeq,
		Type:        mtype,
		Card:        card,
		Cell:        cell,
	}

	res, err := rm.PlayMove(context.Background(), req)
	if err != nil {
		// Rejection: tell the mover only, with current seq for context.
		snap, _ := rm.Snapshot(context.Background(), client.playerID)
		seq := uint64(0)
		if snap.Seq != 0 {
			seq = snap.Seq
		}
		s.sendError(client, seq, mapErrorCode(err), err.Error())
		return
	}

	// Accepted. If it was a duplicate retry, ack only the mover with Duplicate:true.
	if res.Duplicate {
		dto := moveResultToDTO(res)
		env := Envelope{Type: "move_result", Seq: res.Seq, Payload: mustMarshal(dto)}
		select {
		case client.send <- env:
		default:
			s.logger.Warn("dropping duplicate ack", "room", client.roomID, "player", client.playerID)
		}
		return
	}

	// Fresh acceptance: ack the mover and broadcast authoritative state to everyone.
	ack := moveResultToDTO(res)
	ackEnv := Envelope{Type: "move_result", Seq: res.Seq, Payload: mustMarshal(ack)}
	select {
	case client.send <- ackEnv:
	default:
		s.logger.Warn("dropping move ack", "room", client.roomID, "player", client.playerID)
	}
	s.broadcastState(rm)
}

// broadcastState sends a per-viewer "state" envelope to every WS client observing rm.
// It intentionally calls Snapshot per viewer so hands stay private.
func (s *Server) broadcastState(rm *room.Room) {
	clients := s.hubs.clientsFor(rm.ID())
	if len(clients) == 0 {
		return
	}
	for _, c := range clients {
		snap, err := rm.Snapshot(context.Background(), c.playerID)
		if err != nil {
			continue
		}
		dto := snapshotToDTO(snap)
		env := Envelope{Type: "state", Seq: snap.Seq, Payload: mustMarshal(dto)}
		select {
		case c.send <- env:
		default:
			s.logger.Warn("ws send buffer full, dropping state broadcast", "room", c.roomID, "player", c.playerID)
		}
	}
}

func (s *Server) sendError(c *wsClient, seq uint64, code, msg string) {
	env := Envelope{Type: "error", Seq: seq, Payload: mustMarshal(errorDTO(code, msg))}
	select {
	case c.send <- env:
	default:
		s.logger.Warn("dropping error envelope", "room", c.roomID, "player", c.playerID)
	}
}

// ---- HTTP helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorDTO(code, msg))
}

func mapErrorCode(err error) string {
	switch {
	case errors.Is(err, room.ErrRoomClosed):
		return "room_closed"
	case errors.Is(err, room.ErrRoomFull):
		return "room_full"
	case errors.Is(err, room.ErrNotSeated):
		return "not_seated"
	case errors.Is(err, room.ErrGameNotStarted):
		return "game_not_started"
	case errors.Is(err, room.ErrGameFinished):
		return "game_finished"
	case errors.Is(err, room.ErrInvalidPlayerID):
		return "invalid_player_id"
	case errors.Is(err, room.ErrMissingMoveID):
		return "missing_move_id"
	case errors.Is(err, room.ErrStaleSeq):
		return "stale_seq"
	case errors.Is(err, room.ErrNoSuchRoom):
		return "no_such_room"
	case errors.Is(err, room.ErrManagerClosed):
		return "manager_closed"
	case errors.Is(err, engine.ErrGameOver):
		return "game_over"
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
		return "dead_card_used"
	case errors.Is(err, engine.ErrJackNotPlaceable):
		return "jack_not_placeable"
	default:
		return "bad_request"
	}
}

func writeMappedError(w http.ResponseWriter, err error) {
	code := mapErrorCode(err)
	// Map to HTTP status.
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, room.ErrNoSuchRoom):
		status = http.StatusNotFound
	case errors.Is(err, room.ErrRoomClosed):
		status = http.StatusGone
	case errors.Is(err, room.ErrRoomFull):
		status = http.StatusConflict
	case errors.Is(err, room.ErrStaleSeq):
		status = http.StatusConflict
	case errors.Is(err, engine.ErrGameOver):
		status = http.StatusConflict
	}
	writeError(w, status, code, err.Error())
}

// requestLogger is the same middleware as in cmd/tessera/server.go;
// duplicated here so transport can be used without importing main.
func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).String(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker so WebSocket upgrades work through the
// logging middleware. The statusWriter wraps the underlying ResponseWriter but
// must expose the hijacking capability or the gorilla upgrader will fail with
// "response does not implement http.Hijacker" and the dial will be a 500.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("transport: ResponseWriter does not implement http.Hijacker")
}

// Flush implements http.Flusher for completeness (used by some handlers).
func (w *statusWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}
