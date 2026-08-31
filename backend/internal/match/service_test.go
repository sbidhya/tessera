package match

import (
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/room"
)

func newTestService(t *testing.T) (*Service, *room.Manager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Seed: 42}
	manager := room.NewManager(logger, cfg.NewRand)
	service, err := NewService(manager, logger, "unit-test-secret", rand.Reader)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(manager.Shutdown)
	return service, manager
}

func TestIdentityAuthenticatesAndRejectsTampering(t *testing.T) {
	service, manager := newTestService(t)
	identity, err := service.IssueIdentity()
	if err != nil {
		t.Fatalf("IssueIdentity: %v", err)
	}
	if got, err := service.Authenticate(identity.Token); err != nil || got != identity.PlayerID {
		t.Fatalf("Authenticate = %q, %v; want %q, nil", got, err, identity.PlayerID)
	}
	if _, err := service.Authenticate(identity.Token + "x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered token error = %v, want ErrUnauthorized", err)
	}

	// Tokens are self-contained, so a process with the same secret can verify
	// an identity even though its in-memory lobby state is new.
	restarted, err := NewService(manager, nil, "unit-test-secret", rand.Reader)
	if err != nil {
		t.Fatalf("restart service: %v", err)
	}
	if got, err := restarted.Authenticate(identity.Token); err != nil || got != identity.PlayerID {
		t.Fatalf("Authenticate after restart = %q, %v", got, err)
	}
}

func TestMatchmakingPairsFIFOAndReservesDisconnectedSeats(t *testing.T) {
	service, manager := newTestService(t)
	alice, _ := service.IssueIdentity()
	bob, _ := service.IssueIdentity()
	carol, _ := service.IssueIdentity()

	first, err := service.Enqueue(alice.PlayerID, 1)
	if err != nil || first.State != QueueWaiting || first.Position != 1 {
		t.Fatalf("enqueue alice = %+v, %v", first, err)
	}
	otherPool, err := service.Enqueue(carol.PlayerID, 2)
	if err != nil || otherPool.State != QueueWaiting || otherPool.Position != 1 {
		t.Fatalf("enqueue carol = %+v, %v", otherPool, err)
	}
	matched, err := service.Enqueue(bob.PlayerID, 1)
	if err != nil || matched.State != QueueMatched || matched.MatchID == "" {
		t.Fatalf("enqueue bob = %+v, %v", matched, err)
	}
	if aliceStatus := service.QueueStatus(alice.PlayerID); aliceStatus.MatchID != matched.MatchID {
		t.Fatalf("alice status = %+v, bob status = %+v", aliceStatus, matched)
	}

	created, ok := manager.Get(matched.MatchID)
	if !ok {
		t.Fatalf("matched room %q not found", matched.MatchID)
	}
	snapshot, err := created.Snapshot(t.Context(), alice.PlayerID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Status != room.StatusPlaying || len(snapshot.Players) != 2 {
		t.Fatalf("matched room = status %s players %+v", snapshot.Status, snapshot.Players)
	}
	for _, player := range snapshot.Players {
		if player.Present {
			t.Fatalf("matchmaking marked %q present before socket connect", player.ID)
		}
	}
}

func TestQueueCancellationAndOptionConflict(t *testing.T) {
	service, _ := newTestService(t)
	identity, _ := service.IssueIdentity()
	if _, err := service.Enqueue(identity.PlayerID, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := service.Enqueue(identity.PlayerID, 2); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("different-options enqueue error = %v, want ErrAlreadyQueued", err)
	}
	status, err := service.CancelQueue(identity.PlayerID)
	if err != nil || status.State != QueueIdle {
		t.Fatalf("CancelQueue = %+v, %v", status, err)
	}
	if _, err := service.CancelQueue(identity.PlayerID); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("second cancellation error = %v, want ErrNotQueued", err)
	}
}

func TestPresenceCountsConnections(t *testing.T) {
	service, _ := newTestService(t)
	identity, _ := service.IssueIdentity()
	service.Connected(identity.PlayerID)
	service.Connected(identity.PlayerID)
	service.Disconnected(identity.PlayerID)
	if !service.Presence(identity.PlayerID).Online {
		t.Fatal("one remaining connection should keep player online")
	}
	service.Disconnected(identity.PlayerID)
	service.Disconnected(identity.PlayerID) // duplicate cleanup must not underflow
	if service.Presence(identity.PlayerID).Online {
		t.Fatal("player should be offline after final connection closes")
	}
}

func TestConcurrentMatchmakingPairsEveryPlayerOnce(t *testing.T) {
	service, manager := newTestService(t)
	const players = 20
	ids := make([]string, players)
	for i := range ids {
		identity, err := service.IssueIdentity()
		if err != nil {
			t.Fatalf("IssueIdentity %d: %v", i, err)
		}
		ids[i] = identity.PlayerID
	}

	var wg sync.WaitGroup
	errs := make(chan error, players)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Enqueue(id, 1)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Enqueue: %v", err)
		}
	}

	seen := make(map[string]int)
	for _, id := range ids {
		status := service.QueueStatus(id)
		if status.State != QueueMatched || status.MatchID == "" {
			t.Fatalf("status for %s = %+v", id, status)
		}
		seen[status.MatchID]++
	}
	if len(seen) != players/2 || len(manager.List()) != players/2 {
		t.Fatalf("rooms = statuses %d manager %d, want %d", len(seen), len(manager.List()), players/2)
	}
	for matchID, count := range seen {
		if count != 2 {
			t.Errorf("match %s contains %d queued players, want 2", matchID, count)
		}
	}
}
