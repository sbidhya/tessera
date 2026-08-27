package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/room"
)

type testBackend struct {
	api     *Server
	manager *room.Manager
	http    *httptest.Server
}

func newTestBackend(t *testing.T, seed int64) *testBackend {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Seed: seed}
	manager := room.NewManager(logger, cfg.NewRand)
	api := New(manager, logger)
	server := httptest.NewServer(api.Handler())
	b := &testBackend{api: api, manager: manager, http: server}
	t.Cleanup(func() {
		server.Close()
		api.Close()
		manager.Shutdown()
	})
	return b
}

func TestRESTCreateListAndGetState(t *testing.T) {
	b := newTestBackend(t, 1)
	created := createMatch(t, b.http.URL, 1)
	if created.Match.Status != "waiting" || created.Match.Capacity != 2 {
		t.Fatalf("created match = %+v", created.Match)
	}
	if created.Match.SequencesToWin != 1 {
		t.Errorf("sequences_to_win = %d, want 1", created.Match.SequencesToWin)
	}

	resp, err := http.Get(b.http.URL + "/v1/matches")
	if err != nil {
		t.Fatalf("GET matches: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET matches status = %d", resp.StatusCode)
	}
	var listed listMatchesResponse
	decodeResponse(t, resp, &listed)
	if len(listed.Matches) != 1 || listed.Matches[0].ID != created.Match.ID {
		t.Fatalf("listed matches = %+v", listed.Matches)
	}

	resp, err = http.Get(b.http.URL + "/v1/matches/" + created.Match.ID)
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer resp.Body.Close()
	var state stateResponse
	decodeResponse(t, resp, &state)
	if state.Seq != created.Match.Seq || state.State.MatchID != created.Match.ID {
		t.Errorf("state = %+v, create = %+v", state, created.Match)
	}
	if state.State.Viewer != nil || len(state.State.Hand) != 0 {
		t.Error("spectator state exposed a hand")
	}
	if len(state.State.Board) != 100 {
		t.Errorf("board has %d cells, want 100", len(state.State.Board))
	}
}

func TestRESTValidationAndNotFound(t *testing.T) {
	b := newTestBackend(t, 1)

	resp, err := http.Post(b.http.URL+"/v1/matches", "application/json", strings.NewReader(`{"unknown":1}`))
	if err != nil {
		t.Fatalf("POST invalid match: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid JSON status = %d, want 400", resp.StatusCode)
	}

	resp, err = http.Post(b.http.URL+"/v1/matches", "application/json", strings.NewReader(`{"sequences_to_win":-1}`))
	if err != nil {
		t.Fatalf("POST invalid options: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("invalid options status = %d, want 422", resp.StatusCode)
	}

	resp, err = http.Get(b.http.URL + "/v1/matches/r_missing")
	if err != nil {
		t.Fatalf("GET missing match: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing match status = %d, want 404", resp.StatusCode)
	}
}

// TestWebSocketFullGameReconnect is the B3 integration gate. Two real
// WebSocket clients join through the HTTP server, play a complete game from
// their private state messages, retry one move idempotently, lose a socket,
// recover through GET state, reconnect, and finish the match.
func TestWebSocketFullGameReconnect(t *testing.T) {
	b := newTestBackend(t, 7)
	matchID := createMatch(t, b.http.URL, 1).Match.ID

	alice := dialPlayer(t, b.http.URL, matchID, "alice")
	bob := dialPlayer(t, b.http.URL, matchID, "bob")
	defer alice.CloseNow()
	defer bob.CloseNow()

	aliceState := readStateAtLeast(t, alice, 3)
	bobState := readStateAtLeast(t, bob, 3)
	assertPrivateState(t, aliceState.state, 0)
	assertPrivateState(t, bobState.state, 1)

	clients := map[int]*websocket.Conn{0: alice, 1: bob}
	states := map[int]observedState{0: aliceState, 1: bobState}
	moveNumber := 0
	reconnected := false
	duplicateChecked := false

	// A stale move is rejected with a stable protocol code and does not alter
	// state. The client can recover from the error's current sequence.
	firstTurn := states[0].state.Turn
	stalePayload, ok := chooseWireMove(states[firstTurn].state, "stale-attempt")
	if !ok {
		t.Fatal("no opening move available")
	}
	writeMessage(t, clients[firstTurn], Envelope{
		Type: "move", Seq: states[firstTurn].seq - 1, Payload: stalePayload,
	})
	staleError := readError(t, clients[firstTurn])
	if staleError.Code != "stale_seq" || staleError.Seq != states[firstTurn].seq {
		t.Fatalf("stale response = %+v", staleError)
	}

	for states[0].state.Status != "finished" {
		if moveNumber > 5000 {
			t.Fatal("match did not finish in 5000 moves")
		}
		turn := states[0].state.Turn
		current := states[turn]
		payload, ok := chooseWireMove(current.state, fmt.Sprintf("m%d", moveNumber))
		if !ok {
			t.Fatalf("seat %d has no legal move at seq %d", turn, current.seq)
		}
		message := Envelope{Type: "move", Seq: current.seq, Payload: payload}
		writeMessage(t, clients[turn], message)

		nextSeq := current.seq + 1
		states[0] = readStateAtLeast(t, clients[0], nextSeq)
		states[1] = readStateAtLeast(t, clients[1], nextSeq)
		moveNumber++

		// A retry carries the original (now stale) seq. The room's duplicate
		// lookup runs first, so it must replay the original success without
		// applying or broadcasting the move again.
		if !duplicateChecked {
			writeMessage(t, clients[turn], message)
			result := readMoveResult(t, clients[turn], payload.MoveID)
			if !result.Duplicate || result.Seq != nextSeq {
				t.Fatalf("duplicate result = %+v, want duplicate at seq %d", result, nextSeq)
			}
			duplicateChecked = true
		}

		if !reconnected && moveNumber == 3 && states[0].state.Status != "finished" {
			beforeDrop := states[0]
			_ = alice.CloseNow() // abrupt mobile-network-style drop

			bobAfterDrop := readStateAtLeast(t, bob, beforeDrop.seq+1)
			states[1] = bobAfterDrop
			recovered := getPlayerState(t, b.http.URL, matchID, "alice")
			if recovered.Seq != bobAfterDrop.seq {
				t.Fatalf("GET recovery seq = %d, broadcast seq = %d", recovered.Seq, bobAfterDrop.seq)
			}
			if len(recovered.State.Hand) == 0 || playerPresent(recovered.State, "alice") {
				t.Fatalf("recovered alice state missing hand or still present: %+v", recovered.State.Players)
			}

			alice = dialPlayer(t, b.http.URL, matchID, "alice")
			defer alice.CloseNow()
			clients[0] = alice
			states[0] = readStateAtLeast(t, alice, recovered.Seq+1)
			states[1] = readStateAtLeast(t, bob, recovered.Seq+1)
			if !playerPresent(states[0].state, "alice") {
				t.Fatal("alice was not marked present after reconnect")
			}
			reconnected = true
		}
	}

	if !reconnected || !duplicateChecked {
		t.Fatalf("gate coverage: reconnected=%v duplicate_checked=%v", reconnected, duplicateChecked)
	}
	if states[0].state.Winner == nil || states[1].state.Winner == nil {
		t.Fatal("finished state has no winner")
	}
	if *states[0].state.Winner != *states[1].state.Winner {
		t.Fatalf("clients disagree on winner: %d vs %d", *states[0].state.Winner, *states[1].state.Winner)
	}
	t.Logf("winner=%d moves=%d final_seq=%d", *states[0].state.Winner, moveNumber, states[0].seq)
}

type observedState struct {
	seq   uint64
	state State
}

type observedMoveResult struct {
	Seq uint64
	moveResultPayload
}

type observedError struct {
	Seq uint64
	errorPayload
}

func createMatch(t *testing.T, baseURL string, sequencesToWin int) createMatchResponse {
	t.Helper()
	body, err := json.Marshal(createMatchRequest{SequencesToWin: sequencesToWin})
	if err != nil {
		t.Fatalf("marshal create request: %v", err)
	}
	resp, err := http.Post(baseURL+"/v1/matches", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST match: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST match status = %d: %s", resp.StatusCode, data)
	}
	var created createMatchResponse
	decodeResponse(t, resp, &created)
	return created
}

func getPlayerState(t *testing.T, baseURL, matchID, playerID string) stateResponse {
	t.Helper()
	u := baseURL + "/v1/matches/" + matchID + "?player_id=" + url.QueryEscape(playerID)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET player state: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET player state status = %d", resp.StatusCode)
	}
	var state stateResponse
	decodeResponse(t, resp, &state)
	return state
}

func decodeResponse(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func dialPlayer(t *testing.T, baseURL, matchID, playerID string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/v1/matches/" + matchID +
		"/ws?player_id=" + url.QueryEscape(playerID)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		if resp != nil {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("dial %s: %v: %s", playerID, err, data)
		}
		t.Fatalf("dial %s: %v", playerID, err)
	}
	return conn
}

func writeMessage(t *testing.T, conn *websocket.Conn, message Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, message); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
}

func readEnvelope(t *testing.T, conn *websocket.Conn) inboundEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var envelope inboundEnvelope
	if err := wsjson.Read(ctx, conn, &envelope); err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	return envelope
}

func readStateAtLeast(t *testing.T, conn *websocket.Conn, minSeq uint64) observedState {
	t.Helper()
	for {
		envelope := readEnvelope(t, conn)
		switch envelope.Type {
		case "state":
			var state State
			if err := json.Unmarshal(envelope.Payload, &state); err != nil {
				t.Fatalf("decode state: %v", err)
			}
			if envelope.Seq >= minSeq {
				return observedState{seq: envelope.Seq, state: state}
			}
		case "error":
			t.Fatalf("unexpected websocket error at seq %d: %s", envelope.Seq, envelope.Payload)
		}
	}
}

func readMoveResult(t *testing.T, conn *websocket.Conn, moveID string) observedMoveResult {
	t.Helper()
	for {
		envelope := readEnvelope(t, conn)
		if envelope.Type == "error" {
			t.Fatalf("unexpected websocket error at seq %d: %s", envelope.Seq, envelope.Payload)
		}
		if envelope.Type != "move_result" {
			continue
		}
		var result moveResultPayload
		if err := json.Unmarshal(envelope.Payload, &result); err != nil {
			t.Fatalf("decode move result: %v", err)
		}
		if result.MoveID == moveID {
			return observedMoveResult{Seq: envelope.Seq, moveResultPayload: result}
		}
	}
}

func readError(t *testing.T, conn *websocket.Conn) observedError {
	t.Helper()
	for {
		envelope := readEnvelope(t, conn)
		if envelope.Type != "error" {
			continue
		}
		var payload errorPayload
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		return observedError{Seq: envelope.Seq, errorPayload: payload}
	}
}

func assertPrivateState(t *testing.T, state State, viewer int) {
	t.Helper()
	if state.Viewer == nil || *state.Viewer != viewer {
		t.Fatalf("viewer = %v, want %d", state.Viewer, viewer)
	}
	if len(state.Hand) != 7 {
		t.Fatalf("viewer hand = %d cards, want 7", len(state.Hand))
	}
	if len(state.HandCounts) != 2 || state.HandCounts[1-viewer].Count != 7 {
		t.Fatalf("hand counts = %+v", state.HandCounts)
	}
}

func playerPresent(state State, playerID string) bool {
	idx := slices.IndexFunc(state.Players, func(p Player) bool { return p.ID == playerID })
	return idx >= 0 && state.Players[idx].Present
}

func chooseWireMove(state State, moveID string) (movePayload, bool) {
	occupied := make(map[Cell]Chip, len(state.Chips))
	for _, chip := range state.Chips {
		occupied[chip.Cell] = chip
	}
	open := func(cell BoardCell) bool {
		_, used := occupied[cell.Cell]
		return !cell.Corner && !used
	}

	for _, card := range state.Hand {
		switch {
		case isTwoEyed(card):
			for _, cell := range state.Board {
				if open(cell) {
					target := cell.Cell
					return movePayload{MoveID: moveID, Move: "place", Card: card, Cell: &target}, true
				}
			}
		case isOneEyed(card):
			for _, chip := range state.Chips {
				if chip.Owner != *state.Viewer && !chip.InSequence {
					target := chip.Cell
					return movePayload{MoveID: moveID, Move: "remove", Card: card, Cell: &target}, true
				}
			}
		default:
			for _, cell := range state.Board {
				if cell.Card != nil && *cell.Card == card && open(cell) {
					target := cell.Cell
					return movePayload{MoveID: moveID, Move: "place", Card: card, Cell: &target}, true
				}
			}
		}
	}

	for _, card := range state.Hand {
		if card.Rank == "J" {
			continue
		}
		matching, occupiedCount := 0, 0
		for _, cell := range state.Board {
			if cell.Card != nil && *cell.Card == card {
				matching++
				if _, used := occupied[cell.Cell]; used {
					occupiedCount++
				}
			}
		}
		if matching > 0 && matching == occupiedCount {
			return movePayload{MoveID: moveID, Move: "dead_card", Card: card}, true
		}
	}
	return movePayload{}, false
}

func isTwoEyed(card Card) bool {
	return card.Rank == "J" && (card.Suit == "diamonds" || card.Suit == "clubs")
}

func isOneEyed(card Card) bool {
	return card.Rank == "J" && (card.Suit == "hearts" || card.Suit == "spades")
}
