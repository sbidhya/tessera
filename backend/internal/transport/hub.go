package transport

import (
	"sync"

	"github.com/gorilla/websocket"
	"log/slog"
)

// wsClient is a single WebSocket connection to one room as one player.
// One connection = one goroutine reading, one goroutine writing (writeLoop).
type wsClient struct {
	conn     *websocket.Conn
	send     chan Envelope
	roomID   string
	playerID string
	logger   *slog.Logger
}

// writeLoop drains c.send and writes each envelope as a JSON text message.
// It exits when the send channel is closed or the connection fails.
func (c *wsClient) writeLoop(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case env, ok := <-c.send:
			if !ok {
				// Hub closed the channel; tell the peer we are going away.
				_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := c.conn.WriteJSON(env); err != nil {
				c.logger.Debug("ws write failed", "room", c.roomID, "player", c.playerID, "err", err)
				return
			}
		}
	}
}

// hubRegistry tracks active WS connections per room.
// It is intentionally separate from room.Manager: Manager owns the game state,
// the hub owns the set of sockets that observe it. They meet only in the Server
// that wires a successful move to a broadcast.
//
// All methods are concurrency-safe.
type hubRegistry struct {
	mu    sync.Mutex
	rooms map[string]map[*wsClient]struct{}
}

func newHubRegistry() *hubRegistry {
	return &hubRegistry{rooms: make(map[string]map[*wsClient]struct{})}
}

func (h *hubRegistry) add(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.rooms[c.roomID]
	if !ok {
		m = make(map[*wsClient]struct{})
		h.rooms[c.roomID] = m
	}
	m[c] = struct{}{}
}

func (h *hubRegistry) remove(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.rooms[c.roomID]; ok {
		delete(m, c)
		if len(m) == 0 {
			delete(h.rooms, c.roomID)
		}
	}
	// Close send so the writer exits; do not close the conn here (caller owns it).
	close(c.send)
}

func (h *hubRegistry) clientsFor(roomID string) []*wsClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.rooms[roomID]
	if !ok {
		return nil
	}
	out := make([]*wsClient, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	return out
}

func (h *hubRegistry) count(roomID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms[roomID])
}
