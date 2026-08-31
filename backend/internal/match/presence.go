package match

import "sync"

// Presence tracks which players currently hold at least one live WebSocket
// connection. It answers the lobby question "is my opponent still here?"
// without touching any match: the room's per-seat Present flag says whether a
// seat's occupant is connected to THAT match, while Presence says whether the
// player is connected AT ALL.
//
// Connections are refcounted per player id because one player can hold sockets
// in several matches at once; only the last disconnect takes them offline.
// Replacing a socket (a reconnect arriving before the old read loop exits)
// keeps the count steady: the hub reports Online only for genuinely new
// connections, never for a replacement.
//
// This is intentionally not an actor: transitions happen once per socket
// lifetime, not once per move, so a plain mutex is the right tool — the same
// reasoning the room Manager documents for its directory lock.
//
// All methods are safe for concurrent use, including on a nil *Presence, which
// behaves as a disabled tracker. That lets transport hold the pointer
// unconditionally and skip nil checks at every hook site.
type Presence struct {
	mu      sync.Mutex
	sockets map[string]int
}

// NewPresence builds an empty tracker.
func NewPresence() *Presence {
	return &Presence{sockets: make(map[string]int)}
}

// Online records a new live connection for the player. A socket that replaces
// an older one for the same player must NOT call this: the player never went
// offline, so the count must not move.
func (p *Presence) Online(playerID string) {
	if p == nil || playerID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sockets[playerID]++
}

// Offline records a dead connection. It is clamped at zero so a duplicated or
// misordered disconnect can never drive the count negative and poison later
// state.
func (p *Presence) Offline(playerID string) {
	if p == nil || playerID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sockets[playerID] <= 1 {
		delete(p.sockets, playerID)
		return
	}
	p.sockets[playerID]--
}

// IsOnline reports whether the player holds at least one live connection.
func (p *Presence) IsOnline(playerID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sockets[playerID] > 0
}

// Count reports how many distinct players are currently online.
func (p *Presence) Count() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sockets)
}
