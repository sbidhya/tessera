package transport

import (
	"fmt"
	"testing"
	"time"
)

// TestHubRetiresAfterMatchFinishes is the lifecycle regression gate: two
// sockets play a match to completion, both disconnect, and the hub must stop
// its goroutine and leave Server.hubs instead of leaking for the life of the
// process.
func TestHubRetiresAfterMatchFinishes(t *testing.T) {
	b := newTestBackend(t, 42)
	matchID := createMatch(t, b.http.URL, 1).Match.ID

	alice := dialPlayer(t, b.http.URL, matchID, "alice")
	defer alice.CloseNow()
	bob := dialPlayer(t, b.http.URL, matchID, "bob")
	defer bob.CloseNow()

	aliceState := readStateAtLeast(t, alice, 3)
	bobState := readStateAtLeast(t, bob, 3)
	moveNumber := 0
	for aliceState.state.Status != "finished" {
		if moveNumber > 5000 {
			t.Fatal("match did not finish in 5000 moves")
		}
		current := aliceState
		mover := alice
		if aliceState.state.Turn == 1 {
			current = bobState
			mover = bob
		}
		payload, ok := chooseWireMove(current.state, fmt.Sprintf("retire-m%d", moveNumber))
		if !ok {
			t.Fatalf("seat %d has no legal move at seq %d", current.state.Turn, current.seq)
		}
		writeMessage(t, mover, Envelope{Type: "move", Seq: current.seq, Payload: payload})
		nextSeq := current.seq + 1
		aliceState = readStateAtLeast(t, alice, nextSeq)
		bobState = readStateAtLeast(t, bob, nextSeq)
		moveNumber++
	}
	if aliceState.state.Winner == nil {
		t.Fatal("finished state has no winner")
	}

	// Grab the live hub and its done channel while both sockets are still up.
	b.api.mu.Lock()
	hub, ok := b.api.hubs[matchID]
	b.api.mu.Unlock()
	if !ok {
		t.Fatal("no hub registered for the match while sockets are connected")
	}
	done := hub.done

	// Disconnect everyone. Each server read loop then runs unregister; the
	// last one finds an empty hub on a finished match and retires it.
	_ = alice.CloseNow()
	_ = bob.CloseNow()

	// The goroutine must exit: a done-channel assertion, not a sleep.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("hub goroutine did not exit after the last client disconnected")
	}

	b.api.mu.Lock()
	remaining := len(b.api.hubs)
	b.api.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("len(Server.hubs) = %d after match completion, want 0", remaining)
	}
}
