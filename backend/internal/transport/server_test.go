package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/sbidhya/tessera/backend/internal/config"
	matchmaking "github.com/sbidhya/tessera/backend/internal/match"
	"github.com/sbidhya/tessera/backend/internal/room"
)

type testBackend struct {
	api     *Server
	lobby   *matchmaking.Service
	manager *room.Manager
	http    *httptest.Server
}

func newTestBackend(t *testing.T, seed int64) *testBackend {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Seed: seed}
	manager := room.NewManager(logger, cfg.NewRand)
	lobby, err := matchmaking.NewService(manager, logger, "transport-test-secret", rand.Reader)
	if err != nil {
		t.Fatalf("new matchmaking service: %v", err)
	}
	api := New(manager, lobby, logger)
	server := httptest.NewServer(api.Handler())
	b := &testBackend{api: api, lobby: lobby, manager: manager, http: server}
	t.Cleanup(func() {
		server.Close()
		api.Close()
		manager.Shutdown()
	})
	return b
}

func TestDurabilityFailureMapsToServiceUnavailable(t *testing.T) {
	status, code := httpError(room.ErrDurability)
	if status != http.StatusServiceUnavailable || code != "durability_failure" {
		t.Errorf("httpError(ErrDurability) = %d/%q, want %d/durability_failure",
			status, code, http.StatusServiceUnavailable)
	}
	if code := errorCode(room.ErrDurability); code != "durability_failure" {
		t.Errorf("errorCode(ErrDurability) = %q, want durability_failure", code)
	}
}

func TestRESTCreateListAndGetState(t *testing.T) {
	b := newTestBackend(t, 1)
	identity := createIdentity(t, b.http.URL)
	created := createMatch(t, b.http.URL, identity.Token, 1)
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
	identity := createIdentity(t, b.http.URL)

	resp := authenticatedJSONRequest(t, http.MethodPost, b.http.URL+"/v1/matches", identity.Token, strings.NewReader(`{"unknown":1}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid JSON status = %d, want 400", resp.StatusCode)
	}

	resp = authenticatedJSONRequest(t, http.MethodPost, b.http.URL+"/v1/matches", identity.Token, strings.NewReader(`{"sequences_to_win":-1}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("invalid options status = %d, want 422", resp.StatusCode)
	}

	resp, err := http.Get(b.http.URL + "/v1/matches/r_missing")
	if err != nil {
		t.Fatalf("GET missing match: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing match status = %d, want 404", resp.StatusCode)
	}
}

func TestAuthRejectsMissingAndTamperedTokens(t *testing.T) {
	b := newTestBackend(t, 1)

	resp, err := http.Post(b.http.URL+"/v1/matches", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST unauthenticated match: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp.StatusCode)
	}

	identity := createIdentity(t, b.http.URL)
	resp = authenticatedJSONRequest(t, http.MethodPost, b.http.URL+"/v1/matchmaking", identity.Token+"x", strings.NewReader(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered token status = %d, want 401", resp.StatusCode)
	}
}

func TestMatchmakingCanBeCancelled(t *testing.T) {
	b := newTestBackend(t, 1)
	identity := createIdentity(t, b.http.URL)
	queued := joinMatchmaking(t, b.http.URL, identity.Token, 2)
	if queued.Status != "queued" {
		t.Fatalf("initial matchmaking = %+v", queued)
	}
	resp := authenticatedRequest(t, http.MethodDelete, b.http.URL+"/v1/matchmaking", identity.Token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE matchmaking status = %d: %s", resp.StatusCode, data)
	}
	var cancelled matchmakingResponse
	decodeResponse(t, resp, &cancelled)
	if cancelled.Status != "idle" {
		t.Fatalf("cancelled matchmaking = %+v", cancelled)
	}
}

// TestWebSocketFullGameReconnect is the B6 integration gate. Two authenticated
// clients matchmake, play a complete game from their private state messages,
// retry one move idempotently, lose a socket, recover through GET state,
// reconnect, and finish the match.
func TestWebSocketFullGameReconnect(t *testing.T) {
	b := newTestBackend(t, 7)
	aliceIdentity := createIdentity(t, b.http.URL)
	bobIdentity := createIdentity(t, b.http.URL)
	aliceQueued := joinMatchmaking(t, b.http.URL, aliceIdentity.Token, 1)
	if aliceQueued.Status != "queued" || aliceQueued.Position != 1 {
		t.Fatalf("alice matchmaking = %+v, want queued first", aliceQueued)
	}
	bobMatched := joinMatchmaking(t, b.http.URL, bobIdentity.Token, 1)
	if bobMatched.Status != "matched" || bobMatched.MatchID == "" {
		t.Fatalf("bob matchmaking = %+v, want matched", bobMatched)
	}
	aliceMatched := getMatchmaking(t, b.http.URL, aliceIdentity.Token)
	if aliceMatched.Status != "matched" || aliceMatched.MatchID != bobMatched.MatchID {
		t.Fatalf("alice matchmaking = %+v, bob = %+v", aliceMatched, bobMatched)
	}
	matchID := bobMatched.MatchID

	alice := dialPlayer(t, b.http.URL, matchID, aliceIdentity.Token)
	bob := dialPlayer(t, b.http.URL, matchID, bobIdentity.Token)
	defer alice.CloseNow()
	defer bob.CloseNow()

	bobState := readStateAtLeast(t, bob, 1)
	aliceState := readStateAtLeast(t, alice, bobState.seq)
	assertPrivateState(t, aliceState.state, 0)
	assertPrivateState(t, bobState.state, 1)
	if !getPresence(t, b.http.URL, bobIdentity.Token, aliceIdentity.PlayerID).Online {
		t.Fatal("alice should be online after websocket connect")
	}
	resp, err := http.Get(b.http.URL + "/v1/matches/" + matchID + "?player_id=" + aliceIdentity.PlayerID)
	if err != nil {
		t.Fatalf("GET spoofed player state: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET spoofed player state status = %d", resp.StatusCode)
	}
	var spoofed stateResponse
	decodeResponse(t, resp, &spoofed)
	resp.Body.Close()
	if spoofed.State.Viewer != nil || len(spoofed.State.Hand) != 0 {
		t.Fatal("query-string player id exposed private state without a bearer token")
	}

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
			recovered := getPlayerState(t, b.http.URL, matchID, aliceIdentity.Token)
			if recovered.Seq != bobAfterDrop.seq {
				t.Fatalf("GET recovery seq = %d, broadcast seq = %d", recovered.Seq, bobAfterDrop.seq)
			}
			if len(recovered.State.Hand) == 0 || playerPresent(recovered.State, aliceIdentity.PlayerID) {
				t.Fatalf("recovered alice state missing hand or still present: %+v", recovered.State.Players)
			}
			if getPresence(t, b.http.URL, bobIdentity.Token, aliceIdentity.PlayerID).Online {
				t.Fatal("alice should be offline after websocket disconnect")
			}

			alice = dialPlayer(t, b.http.URL, matchID, aliceIdentity.Token)
			defer alice.CloseNow()
			clients[0] = alice
			states[0] = readStateAtLeast(t, alice, recovered.Seq+1)
			states[1] = readStateAtLeast(t, bob, recovered.Seq+1)
			if !playerPresent(states[0].state, aliceIdentity.PlayerID) {
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

func createIdentity(t *testing.T, baseURL string) identityResponse {
	t.Helper()
	resp, err := http.Post(baseURL+"/v1/auth/anonymous", "application/json", nil)
	if err != nil {
		t.Fatalf("POST anonymous auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST anonymous auth status = %d: %s", resp.StatusCode, data)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous auth Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	var identity identityResponse
	decodeResponse(t, resp, &identity)
	if identity.PlayerID == "" || identity.Token == "" {
		t.Fatalf("incomplete identity: %+v", identity)
	}
	return identity
}

func createMatch(t *testing.T, baseURL, token string, sequencesToWin int) createMatchResponse {
	t.Helper()
	body, err := json.Marshal(createMatchRequest{SequencesToWin: sequencesToWin})
	if err != nil {
		t.Fatalf("marshal create request: %v", err)
	}
	resp := authenticatedJSONRequest(t, http.MethodPost, baseURL+"/v1/matches", token, bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST match status = %d: %s", resp.StatusCode, data)
	}
	var created createMatchResponse
	decodeResponse(t, resp, &created)
	return created
}

func joinMatchmaking(t *testing.T, baseURL, token string, sequencesToWin int) matchmakingResponse {
	t.Helper()
	body, err := json.Marshal(matchmakingRequest{SequencesToWin: sequencesToWin})
	if err != nil {
		t.Fatalf("marshal matchmaking request: %v", err)
	}
	resp := authenticatedJSONRequest(t, http.MethodPost, baseURL+"/v1/matchmaking", token, bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST matchmaking status = %d: %s", resp.StatusCode, data)
	}
	var status matchmakingResponse
	decodeResponse(t, resp, &status)
	return status
}

func getMatchmaking(t *testing.T, baseURL, token string) matchmakingResponse {
	t.Helper()
	resp := authenticatedRequest(t, http.MethodGet, baseURL+"/v1/matchmaking", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET matchmaking status = %d: %s", resp.StatusCode, data)
	}
	var status matchmakingResponse
	decodeResponse(t, resp, &status)
	return status
}

func getPresence(t *testing.T, baseURL, token, playerID string) presenceResponse {
	t.Helper()
	resp := authenticatedRequest(t, http.MethodGet, baseURL+"/v1/presence/"+playerID, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET presence status = %d: %s", resp.StatusCode, data)
	}
	var presence presenceResponse
	decodeResponse(t, resp, &presence)
	return presence
}

func getPlayerState(t *testing.T, baseURL, matchID, token string) stateResponse {
	t.Helper()
	resp := authenticatedRequest(t, http.MethodGet, baseURL+"/v1/matches/"+matchID, token, nil)
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

func dialPlayer(t *testing.T, baseURL, matchID, token string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/v1/matches/" + matchID + "/ws"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": []string{"Bearer " + token},
	}})
	if err != nil {
		if resp != nil {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("dial websocket: %v: %s", err, data)
		}
		t.Fatalf("dial websocket: %v", err)
	}
	return conn
}

func authenticatedJSONRequest(t *testing.T, method, requestURL, token string, body io.Reader) *http.Response {
	t.Helper()
	return authenticatedRequest(t, method, requestURL, token, body, "application/json")
}

func authenticatedRequest(t *testing.T, method, requestURL, token string, body io.Reader, contentType ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, requestURL, body)
	if err != nil {
		t.Fatalf("build %s request: %v", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if len(contentType) > 0 {
		req.Header.Set("Content-Type", contentType[0])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, requestURL, err)
	}
	return resp
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
