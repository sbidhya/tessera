// Package match owns process-local player identity, matchmaking, and presence.
//
// It sits outside the room layer: rooms remain authoritative over game state,
// while this package decides which authenticated players should be seated
// together and tracks whether they currently have a live connection.
package match

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

const (
	playerIDBytes = 16
	tokenVersion  = "v1"
)

var (
	ErrUnauthorized     = errors.New("match: invalid or missing bearer token")
	ErrAlreadyQueued    = errors.New("match: player is already queued with different options")
	ErrAlreadyMatched   = errors.New("match: player is already matched")
	ErrNotQueued        = errors.New("match: player is not queued")
	ErrInvalidMatchOpts = errors.New("match: sequences_to_win must be positive")
)

type Identity struct {
	PlayerID string
	Token    string
}

type QueueState string

const (
	QueueIdle    QueueState = "idle"
	QueueWaiting QueueState = "queued"
	QueueMatched QueueState = "matched"
)

type QueueStatus struct {
	State          QueueState
	MatchID        string
	Position       int
	SequencesToWin int
}

type Presence struct {
	PlayerID string
	Online   bool
}

type playerState struct {
	queueState     QueueState
	matchID        string
	sequencesToWin int
	connections    int
}

// Service is the small B6 coordination layer. Its mutex protects only lobby
// metadata; it is never used on the per-move hot path owned by room actors.
type Service struct {
	manager *room.Manager
	logger  *slog.Logger
	secret  []byte
	entropy io.Reader

	entropyMu sync.Mutex
	mu        sync.Mutex
	players   map[string]*playerState
	queues    map[int][]string
}

func NewService(manager *room.Manager, logger *slog.Logger, secret string, entropy io.Reader) (*Service, error) {
	if manager == nil {
		return nil, errors.New("match: room manager is required")
	}
	if secret == "" {
		return nil, errors.New("match: auth secret is required")
	}
	if entropy == nil {
		return nil, errors.New("match: entropy source is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		manager: manager,
		logger:  logger,
		secret:  []byte(secret),
		entropy: entropy,
		players: make(map[string]*playerState),
		queues:  make(map[int][]string),
	}, nil
}

// IssueIdentity creates an opaque public player id and a signed bearer token.
// Authentication entropy deliberately comes from crypto/rand in production;
// unlike game shuffles, credentials must not be reproducible from TESSERA_SEED.
func (s *Service) IssueIdentity() (Identity, error) {
	for {
		var raw [playerIDBytes]byte
		s.entropyMu.Lock()
		_, err := io.ReadFull(s.entropy, raw[:])
		s.entropyMu.Unlock()
		if err != nil {
			return Identity{}, fmt.Errorf("match: create player id: %w", err)
		}

		playerID := "p_" + hex.EncodeToString(raw[:])
		s.mu.Lock()
		if _, exists := s.players[playerID]; exists {
			s.mu.Unlock()
			continue
		}
		s.players[playerID] = &playerState{queueState: QueueIdle}
		s.mu.Unlock()

		return Identity{PlayerID: playerID, Token: s.sign(playerID)}, nil
	}
}

// Authenticate verifies a self-contained bearer token. Because identity is
// signed rather than stored only in memory, the same token remains valid after
// a process restart as long as TESSERA_AUTH_SECRET is stable.
func (s *Service) Authenticate(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != tokenVersion || !validPlayerID(parts[1]) {
		return "", ErrUnauthorized
	}
	want := s.signature(parts[1])
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, want) {
		return "", ErrUnauthorized
	}

	s.mu.Lock()
	if _, ok := s.players[parts[1]]; !ok {
		s.players[parts[1]] = &playerState{queueState: QueueIdle}
	}
	s.mu.Unlock()
	return parts[1], nil
}

func (s *Service) sign(playerID string) string {
	return tokenVersion + "." + playerID + "." + base64.RawURLEncoding.EncodeToString(s.signature(playerID))
}

func (s *Service) signature(playerID string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = io.WriteString(mac, tokenVersion+":"+playerID)
	return mac.Sum(nil)
}

func validPlayerID(playerID string) bool {
	if len(playerID) != 2+playerIDBytes*2 || !strings.HasPrefix(playerID, "p_") {
		return false
	}
	_, err := hex.DecodeString(playerID[2:])
	return err == nil
}

// Enqueue places a player in the FIFO pool for the requested win condition.
// When a compatible waiter exists, both players are durably seated in a newly
// created room before either is reported as matched.
func (s *Service) Enqueue(playerID string, sequencesToWin int) (QueueStatus, error) {
	if sequencesToWin == 0 {
		sequencesToWin = 2
	}
	if sequencesToWin < 1 {
		return QueueStatus{}, ErrInvalidMatchOpts
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	player := s.ensurePlayerLocked(playerID)
	switch player.queueState {
	case QueueMatched:
		return s.statusLocked(playerID, player), nil
	case QueueWaiting:
		if player.sequencesToWin != sequencesToWin {
			return QueueStatus{}, ErrAlreadyQueued
		}
		return s.statusLocked(playerID, player), nil
	}

	opponent := s.takeWaiterLocked(sequencesToWin, playerID)
	if opponent == "" {
		player.queueState = QueueWaiting
		player.sequencesToWin = sequencesToWin
		s.queues[sequencesToWin] = append(s.queues[sequencesToWin], playerID)
		return s.statusLocked(playerID, player), nil
	}

	created, err := s.manager.Create(engine.Options{NumPlayers: 2, SequencesToWin: sequencesToWin})
	if err != nil {
		s.queues[sequencesToWin] = append([]string{opponent}, s.queues[sequencesToWin]...)
		return QueueStatus{}, err
	}
	joinCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := created.Join(joinCtx, opponent); err != nil {
		s.queues[sequencesToWin] = append([]string{opponent}, s.queues[sequencesToWin]...)
		return QueueStatus{}, err
	}
	if _, err := created.Join(joinCtx, playerID); err != nil {
		s.queues[sequencesToWin] = append([]string{opponent}, s.queues[sequencesToWin]...)
		return QueueStatus{}, err
	}
	// Matchmaking reserves seats, but it is not itself a live connection. Once
	// both seats are filled (and therefore held for reconnect), mark them absent
	// until their authenticated WebSockets actually connect.
	if err := created.Leave(joinCtx, opponent); err != nil {
		return QueueStatus{}, err
	}
	if err := created.Leave(joinCtx, playerID); err != nil {
		return QueueStatus{}, err
	}

	matchID := created.ID()
	for _, id := range []string{opponent, playerID} {
		state := s.ensurePlayerLocked(id)
		state.queueState = QueueMatched
		state.matchID = matchID
		state.sequencesToWin = sequencesToWin
	}
	s.logger.Info("players matched", "match", matchID, "player_1", opponent, "player_2", playerID)
	return s.statusLocked(playerID, player), nil
}

func (s *Service) QueueStatus(playerID string) QueueStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	player := s.ensurePlayerLocked(playerID)
	return s.statusLocked(playerID, player)
}

func (s *Service) CancelQueue(playerID string) (QueueStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	player := s.ensurePlayerLocked(playerID)
	switch player.queueState {
	case QueueMatched:
		return QueueStatus{}, ErrAlreadyMatched
	case QueueIdle:
		return QueueStatus{}, ErrNotQueued
	}
	queue := s.queues[player.sequencesToWin]
	queue = slices.DeleteFunc(queue, func(id string) bool { return id == playerID })
	if len(queue) == 0 {
		delete(s.queues, player.sequencesToWin)
	} else {
		s.queues[player.sequencesToWin] = queue
	}
	player.queueState = QueueIdle
	player.sequencesToWin = 0
	return s.statusLocked(playerID, player), nil
}

func (s *Service) Connected(playerID string) {
	s.mu.Lock()
	s.ensurePlayerLocked(playerID).connections++
	s.mu.Unlock()
}

func (s *Service) Disconnected(playerID string) {
	s.mu.Lock()
	player := s.ensurePlayerLocked(playerID)
	if player.connections > 0 {
		player.connections--
	}
	s.mu.Unlock()
}

func (s *Service) Presence(playerID string) Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	player := s.players[playerID]
	return Presence{PlayerID: playerID, Online: player != nil && player.connections > 0}
}

func (s *Service) ensurePlayerLocked(playerID string) *playerState {
	player := s.players[playerID]
	if player == nil {
		player = &playerState{queueState: QueueIdle}
		s.players[playerID] = player
	}
	return player
}

func (s *Service) takeWaiterLocked(sequencesToWin int, exclude string) string {
	queue := s.queues[sequencesToWin]
	for len(queue) > 0 {
		candidate := queue[0]
		queue = queue[1:]
		s.queues[sequencesToWin] = queue
		state := s.players[candidate]
		if candidate != exclude && state != nil && state.queueState == QueueWaiting && state.sequencesToWin == sequencesToWin {
			return candidate
		}
	}
	return ""
}

func (s *Service) statusLocked(playerID string, player *playerState) QueueStatus {
	status := QueueStatus{
		State:          player.queueState,
		MatchID:        player.matchID,
		SequencesToWin: player.sequencesToWin,
	}
	if player.queueState == QueueWaiting {
		position := slices.Index(s.queues[player.sequencesToWin], playerID)
		if position >= 0 {
			status.Position = position + 1
		}
	}
	return status
}
