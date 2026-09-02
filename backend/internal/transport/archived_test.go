package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestGetStateArchivedFallback pins the post-eviction contract: a live miss
// that the cold tier knows about answers 410 match_archived (not 404), while
// an unknown id stays 404 and a cold-tier error fails closed to 404.
func TestGetStateArchivedFallback(t *testing.T) {
	b := newTestBackend(t, 101)
	created := createMatch(t, b.http.URL, 1)
	matchID := created.Match.ID

	// Simulate a retention eviction: the room is gone from the manager.
	if err := b.manager.Close(matchID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	getCode := func() (int, string) {
		resp, err := http.Get(b.http.URL + "/v1/matches/" + matchID)
		if err != nil {
			t.Fatalf("GET state: %v", err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var decoded errorResponse
		_ = json.Unmarshal(data, &decoded)
		return resp.StatusCode, decoded.Error.Code
	}

	// No cold-tier lookup wired: every miss is 404.
	if status, code := getCode(); status != http.StatusNotFound || code != "match_not_found" {
		t.Fatalf("without lookup: status/code = %d/%q, want 404/match_not_found", status, code)
	}

	// Cold tier knows the match: 410 with a stable code.
	b.api.isArchived = func(context.Context, string) (bool, error) { return true, nil }
	if status, code := getCode(); status != http.StatusGone || code != "match_archived" {
		t.Fatalf("archived: status/code = %d/%q, want 410/match_archived", status, code)
	}

	// Cold tier does not know it: still 404.
	b.api.isArchived = func(context.Context, string) (bool, error) { return false, nil }
	if status, code := getCode(); status != http.StatusNotFound || code != "match_not_found" {
		t.Fatalf("unknown: status/code = %d/%q, want 404/match_not_found", status, code)
	}

	// Cold-tier error fails closed to 404 rather than 500/410.
	b.api.isArchived = func(context.Context, string) (bool, error) {
		return false, errors.New("sqlite is sick")
	}
	if status, _ := getCode(); status != http.StatusNotFound {
		t.Fatalf("lookup error: status = %d, want 404", status)
	}

	// A never-existed id is 404 even when the lookup claims nothing.
	resp, err := http.Get(b.http.URL + "/v1/matches/r_nope")
	if err != nil {
		t.Fatalf("GET unknown: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", resp.StatusCode)
	}
}

// TestCloseHubStopsHub verifies the P0-1 coordination primitive: closing the
// hub for an evicted room removes it from the directory and stops its
// goroutine. It is a no-op for unknown ids.
func TestCloseHubStopsHub(t *testing.T) {
	b := newTestBackend(t, 102)
	created := createMatch(t, b.http.URL, 1)
	match, ok := b.manager.Get(created.Match.ID)
	if !ok {
		t.Fatal("created room missing from manager")
	}
	hub, err := b.api.hubFor(match)
	if err != nil {
		t.Fatalf("hubFor: %v", err)
	}
	done := hub.done

	b.api.mu.Lock()
	if len(b.api.hubs) != 1 {
		b.api.mu.Unlock()
		t.Fatalf("hubs = %d, want 1", len(b.api.hubs))
	}
	b.api.mu.Unlock()

	b.api.CloseHub(match.ID())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub goroutine did not exit after CloseHub")
	}
	b.api.mu.Lock()
	remaining := len(b.api.hubs)
	b.api.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("hubs = %d after CloseHub, want 0", remaining)
	}

	// Unknown ids are a no-op, not a panic.
	b.api.CloseHub("r_nope")
}

// TestManagerEvictHookClosesHub ties the two together: the manager's evict
// hook wired to Server.CloseHub retires the hub when the room goes away, so
// retention eviction cannot leak the transport side.
func TestManagerEvictHookClosesHub(t *testing.T) {
	b := newTestBackend(t, 103)
	b.manager.SetEvictHook(b.api.CloseHub)

	created := createMatch(t, b.http.URL, 1)
	match, ok := b.manager.Get(created.Match.ID)
	if !ok {
		t.Fatal("created room missing from manager")
	}
	hub, err := b.api.hubFor(match)
	if err != nil {
		t.Fatalf("hubFor: %v", err)
	}
	done := hub.done

	// An explicit Close stands in for a retention eviction: both funnel
	// through the same hook-after-close path.
	if err := b.manager.Close(match.ID()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub goroutine did not exit after room eviction")
	}
	b.api.mu.Lock()
	remaining := len(b.api.hubs)
	b.api.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("hubs = %d after room eviction, want 0", remaining)
	}
}
