package transport

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	matchmaking "github.com/sbidhya/tessera/backend/internal/match"
	"github.com/sbidhya/tessera/backend/internal/room"
)

const outboundQueueSize = 64

var errClientReplaced = errors.New("websocket connection was replaced")

type wsClient struct {
	playerID string
	conn     *websocket.Conn
	logger   *slog.Logger
	send     chan Envelope
	done     chan struct{}
	closeOne sync.Once
}

func newWSClient(playerID string, conn *websocket.Conn, logger *slog.Logger) *wsClient {
	return &wsClient{
		playerID: playerID,
		conn:     conn,
		logger:   logger,
		send:     make(chan Envelope, outboundQueueSize),
		done:     make(chan struct{}),
	}
}

func (c *wsClient) enqueue(message Envelope) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.send <- message:
		return true
	case <-c.done:
		return false
	default:
		return false
	}
}

func (c *wsClient) writeLoop() {
	for {
		select {
		case message := <-c.send:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := wsjson.Write(ctx, c.conn, message)
			cancel()
			if err != nil {
				c.logger.Debug("websocket write ended", "err", err)
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *wsClient) close() {
	c.closeOne.Do(func() {
		close(c.done)
		_ = c.conn.CloseNow()
	})
}

type registerOp struct {
	client *wsClient
	reply  chan error
}

type unregisterOp struct {
	client *wsClient
	reply  chan struct{}
}

type moveOp struct {
	client  *wsClient
	request room.MoveRequest
	reply   chan moveOutcome
}

type seqOp struct {
	reply chan uint64
}

type moveOutcome struct {
	seq uint64
	err error
}

// matchHub serializes transport-side events for one room. Room methods are
// already actors; this second, outer actor supplies the ordering boundary for
// connection membership and broadcasts, preventing concurrent socket readers
// from publishing seq N+1 before seq N.
type matchHub struct {
	match  *room.Room
	lobby  *matchmaking.Service
	logger *slog.Logger
	ops    chan any
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	// Owned only by loop.
	clients  map[*wsClient]struct{}
	byPlayer map[string]*wsClient
	lastSeq  uint64
}

func newMatchHub(match *room.Room, lobby *matchmaking.Service, logger *slog.Logger) *matchHub {
	h := &matchHub{
		match:    match,
		lobby:    lobby,
		logger:   logger.With("match", match.ID()),
		ops:      make(chan any, 64),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		clients:  make(map[*wsClient]struct{}),
		byPlayer: make(map[string]*wsClient),
	}
	go h.loop()
	return h
}

func (h *matchHub) loop() {
	defer close(h.done)
	for {
		select {
		case <-h.stop:
			for client := range h.clients {
				_ = h.match.Leave(context.Background(), client.playerID)
				h.lobby.Disconnected(client.playerID)
				client.close()
			}
			return
		case raw := <-h.ops:
			switch op := raw.(type) {
			case registerOp:
				op.reply <- h.handleRegister(op.client)
			case unregisterOp:
				h.handleUnregister(op.client)
				op.reply <- struct{}{}
			case moveOp:
				op.reply <- h.handleMove(op.client, op.request)
			case seqOp:
				op.reply <- h.snapshotSeq()
			}
		}
	}
}

func (h *matchHub) handleRegister(client *wsClient) error {
	join, err := h.match.Join(context.Background(), client.playerID)
	if err != nil {
		return err
	}
	if old := h.byPlayer[client.playerID]; old != nil && old != client {
		delete(h.clients, old)
		h.lobby.Disconnected(old.playerID)
		old.close()
	}
	h.clients[client] = struct{}{}
	h.byPlayer[client.playerID] = client
	h.lobby.Connected(client.playerID)
	h.lastSeq = join.Seq
	h.broadcastStates()
	h.logger.Info("websocket connected", "player", client.playerID, "seq", h.lastSeq)
	return nil
}

func (h *matchHub) handleUnregister(client *wsClient) {
	if h.byPlayer[client.playerID] != client {
		return // a newer connection already replaced this one
	}
	delete(h.byPlayer, client.playerID)
	delete(h.clients, client)
	h.lobby.Disconnected(client.playerID)
	if err := h.match.Leave(context.Background(), client.playerID); err != nil && !errors.Is(err, room.ErrRoomClosed) {
		h.logger.Warn("leave after websocket disconnect failed", "player", client.playerID, "err", err)
		return
	}
	h.broadcastStates()
	h.logger.Info("websocket disconnected", "player", client.playerID, "seq", h.lastSeq)
}

func (h *matchHub) handleMove(client *wsClient, request room.MoveRequest) moveOutcome {
	if h.byPlayer[client.playerID] != client {
		return moveOutcome{seq: h.snapshotSeq(), err: errClientReplaced}
	}
	result, err := h.match.PlayMove(context.Background(), request)
	if err != nil {
		return moveOutcome{seq: h.snapshotSeq(), err: err}
	}
	h.lastSeq = result.Seq
	message := Envelope{
		Type: "move_result",
		Seq:  result.Seq,
		Payload: moveResultPayload{
			MoveID:    request.MoveID,
			PlayerID:  request.PlayerID,
			Duplicate: result.Duplicate,
			Status:    result.Status.String(),
			Turn:      int(result.Turn),
			Winner:    optionalPlayer(result.Winner),
		},
	}
	if result.Duplicate {
		if !client.enqueue(message) {
			client.close()
		}
		return moveOutcome{seq: result.Seq}
	}
	h.broadcast(message)
	h.broadcastStates()
	return moveOutcome{seq: result.Seq}
}

func (h *matchHub) broadcast(message Envelope) {
	for client := range h.clients {
		if !client.enqueue(message) {
			h.drop(client)
		}
	}
}

// broadcastStates renders a separate snapshot for every connection so only a
// player's own hand crosses that connection. If a slow client is dropped, its
// Leave changes seq; repeat once more so remaining clients see that presence
// transition too.
func (h *matchHub) broadcastStates() {
	for {
		dropped := false
		for client := range h.clients {
			snap, err := h.match.Snapshot(context.Background(), client.playerID)
			if err != nil {
				h.logger.Warn("snapshot for broadcast failed", "player", client.playerID, "err", err)
				continue
			}
			h.lastSeq = snap.Seq
			if !client.enqueue(Envelope{Type: "state", Seq: snap.Seq, Payload: stateFromSnapshot(snap)}) {
				h.drop(client)
				dropped = true
			}
		}
		if !dropped {
			return
		}
	}
}

func (h *matchHub) drop(client *wsClient) {
	delete(h.clients, client)
	if h.byPlayer[client.playerID] == client {
		delete(h.byPlayer, client.playerID)
		h.lobby.Disconnected(client.playerID)
		if err := h.match.Leave(context.Background(), client.playerID); err != nil && !errors.Is(err, room.ErrRoomClosed) {
			h.logger.Warn("leave for slow websocket failed", "player", client.playerID, "err", err)
		}
	}
	client.close()
}

func (h *matchHub) snapshotSeq() uint64 {
	snap, err := h.match.Snapshot(context.Background(), "")
	if err == nil {
		h.lastSeq = snap.Seq
	}
	return h.lastSeq
}

func (h *matchHub) register(ctx context.Context, client *wsClient) error {
	reply := make(chan error, 1)
	if !h.submit(ctx, registerOp{client: client, reply: reply}) {
		return room.ErrRoomClosed
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-h.done:
		return room.ErrRoomClosed
	}
}

func (h *matchHub) unregister(ctx context.Context, client *wsClient) {
	reply := make(chan struct{}, 1)
	if !h.submit(ctx, unregisterOp{client: client, reply: reply}) {
		return
	}
	select {
	case <-reply:
	case <-ctx.Done():
	case <-h.done:
	}
}

func (h *matchHub) move(ctx context.Context, client *wsClient, request room.MoveRequest) moveOutcome {
	reply := make(chan moveOutcome, 1)
	if !h.submit(ctx, moveOp{client: client, request: request, reply: reply}) {
		return moveOutcome{err: room.ErrRoomClosed}
	}
	select {
	case result := <-reply:
		return result
	case <-ctx.Done():
		return moveOutcome{err: ctx.Err()}
	case <-h.done:
		return moveOutcome{err: room.ErrRoomClosed}
	}
}

func (h *matchHub) currentSeq() uint64 {
	reply := make(chan uint64, 1)
	if !h.submit(context.Background(), seqOp{reply: reply}) {
		return 0
	}
	select {
	case seq := <-reply:
		return seq
	case <-h.done:
		return 0
	}
}

func (h *matchHub) submit(ctx context.Context, op any) bool {
	select {
	case h.ops <- op:
		return true
	case <-ctx.Done():
		return false
	case <-h.done:
		return false
	}
}

func (h *matchHub) Close() {
	h.once.Do(func() { close(h.stop) })
	<-h.done
}
