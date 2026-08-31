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
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sbidhya/tessera/backend/internal/auth"
	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/match"
	"github.com/sbidhya/tessera/backend/internal/room"
)

// b6Backend is a transport server with the full B6 stack: identities,
// matchmaking, and presence. newTestBackend (legacy, no deps) is untouched,
// so the B3 tests keep proving the no-auth behavior is preserved.
type b6Backend struct {
	api        *Server
	manager    *room.Manager
	matchmaker *match.Matchmaker
	http       *httptest.Server
}

func newTestB6Backend(t *testing.T, seed int64) *b6Backend {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Seed: seed}
	manager := room.NewManager(logger, cfg.NewRand)
	authenticator := auth.New([]byte("test-secret"), cfg.NewRand("player-ids"))
	mm := match.NewMatchmaker(logger, manager)
	presence := match.NewPresence()
	api := NewWithDeps(manager, logger, Deps{Auth: authenticator, Matchmaker: mm, Presence: presence})
	server := httptest.NewServer(api.Handler())
	b := &b6Backend{api: api, manager: manager, matchmaker: mm, http: server}
	t.Cleanup(func() {
		server.Close()
		api.Close()
		mm.Close()
		manager.Shutdown()
	})
	return b
}

type credential struct {
	id    string
	token string
}

func postJSON(url string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	resp, err := http.Post(url, "application/json", reader)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func createPlayer(t *testing.T, baseURL string) credential {
	t.Helper()
	status, data, err := postJSON(baseURL+"/v1/players", nil)
	if err != nil {
		t.Fatalf("POST players: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("POST players status = %d: %s", status, data)
	}
	var resp createPlayerResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode player: %v", err)
	}
	if resp.PlayerID == "" || resp.Token == "" {
		t.Fatalf("incomplete identity: %+v", resp)
	}
	return credential{id: resp.PlayerID, token: resp.Token}
}

type mmJoinOutcome struct {
	resp   joinMatchmakingResponse
	status int
	err    error
}

func postMatchmakingJoin(baseURL string, body any) mmJoinOutcome {
	status, data, err := postJSON(baseURL+"/v1/matchmaking/join", body)
	if err != nil {
		return mmJoinOutcome{err: err}
	}
	out := mmJoinOutcome{status: status}
	if status == http.StatusOK {
		out.err = json.Unmarshal(data, &out.resp)
	}
	return out
}

func joinAsync(baseURL string, body any) <-chan mmJoinOutcome {
	ch := make(chan mmJoinOutcome, 1)
	go func() { ch <- postMatchmakingJoin(baseURL, body) }()
	return ch
}

func joinRequest(cred credential, sequencesToWin int) joinMatchmakingRequest {
	return joinMatchmakingRequest{PlayerID: cred.id, Token: cred.token, SequencesToWin: sequencesToWin}
}

func getMatchmakingStatus(t *testing.T, baseURL string) int {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/matchmaking/status")
	if err != nil {
		t.Fatalf("GET matchmaking status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("matchmaking status = %d", resp.StatusCode)
	}
	var status matchmakingStatusResponse
	decodeResponse(t, resp, &status)
	return status.Waiting
}

func waitForQueueDepth(t *testing.T, baseURL string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := getMatchmakingStatus(t, baseURL); n == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("queue depth never reached %d", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getPresenceCount(t *testing.T, baseURL string) int {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/presence")
	if err != nil {
		t.Fatalf("GET presence: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presence status = %d", resp.StatusCode)
	}
	var presence presenceResponse
	decodeResponse(t, resp, &presence)
	return presence.Online
}

func getPlayerOnline(t *testing.T, baseURL, playerID string) bool {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/presence/" + url.PathEscape(playerID))
	if err != nil {
		t.Fatalf("GET player presence: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("player presence status = %d", resp.StatusCode)
	}
	var presence playerPresenceResponse
	decodeResponse(t, resp, &presence)
	return presence.Online
}

func waitForPresence(t *testing.T, baseURL string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := getPresenceCount(t, baseURL); n == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("presence never reached %d", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func dialPlayerWithToken(t *testing.T, baseURL, matchID string, cred credential) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/v1/matches/" + matchID +
		"/ws?player_id=" + url.QueryEscape(cred.id) + "&token=" + url.QueryEscape(cred.token)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		if resp != nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("dial %s: %v: %s", cred.id, err, data)
		}
		t.Fatalf("dial %s: %v", cred.id, err)
	}
	return conn
}

func getStateWithToken(t *testing.T, baseURL, matchID string, cred credential) stateResponse {
	t.Helper()
	u := baseURL + "/v1/matches/" + matchID + "?player_id=" + url.QueryEscape(cred.id) +
		"&token=" + url.QueryEscape(cred.token)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET state status = %d", resp.StatusCode)
	}
	var state stateResponse
	decodeResponse(t, resp, &state)
	return state
}

func getStateStatus(baseURL, matchID, playerID, token string) int {
	u := baseURL + "/v1/matches/" + matchID
	if playerID != "" {
		u += "?player_id=" + url.QueryEscape(playerID) + "&token=" + url.QueryEscape(token)
	}
	resp, err := http.Get(u)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestB6TokenEnforcement pins the auth boundary: private views and sockets
// need a valid token, spectators stay open, and the legacy no-auth server
// keeps its old behavior (covered by the B3 suite, asserted explicitly here).
func TestB6TokenEnforcement(t *testing.T) {
	b := newTestB6Backend(t, 21)
	alice := createPlayer(t, b.http.URL)
	bob := createPlayer(t, b.http.URL)
	if alice.id == bob.id {
		t.Fatal("two players received the same id")
	}

	// An identity unlocks match creation in auth mode: no identity at all is a
	// 400, a wrong token for a claimed id is a 401.
	status, _, err := postJSON(b.http.URL+"/v1/matches", map[string]any{"sequences_to_win": 1})
	if err != nil || status != http.StatusBadRequest {
		t.Fatalf("anonymous create status = %d, %v; want 400", status, err)
	}
	status, data, err := postJSON(b.http.URL+"/v1/matches",
		createMatchRequest{SequencesToWin: 1, PlayerID: alice.id, Token: alice.token})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("authenticated create status = %d, %v: %s", status, err, data)
	}
	var created createMatchResponse
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatalf("decode created match: %v", err)
	}
	matchID := created.Match.ID

	// Spectator state stays public; private state needs the token.
	if s := getStateStatus(b.http.URL, matchID, "", ""); s != http.StatusOK {
		t.Fatalf("spectator state status = %d, want 200", s)
	}
	if s := getStateStatus(b.http.URL, matchID, alice.id, ""); s != http.StatusUnauthorized {
		t.Fatalf("tokenless private state status = %d, want 401", s)
	}
	if s := getStateStatus(b.http.URL, matchID, alice.id, "forged"); s != http.StatusUnauthorized {
		t.Fatalf("forged-token private state status = %d, want 401", s)
	}
	// Bob's valid token does not unlock Alice's private view.
	if s := getStateStatus(b.http.URL, matchID, alice.id, bob.token); s != http.StatusUnauthorized {
		t.Fatalf("cross-player token state status = %d, want 401", s)
	}

	// WebSocket upgrade rejects bad credentials with an HTTP status.
	for name, token := range map[string]string{"missing": "", "forged": "forged", "cross-player": bob.token} {
		wsURL := "ws" + strings.TrimPrefix(b.http.URL, "http") + "/v1/matches/" + matchID +
			"/ws?player_id=" + url.QueryEscape(alice.id) + "&token=" + url.QueryEscape(token)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, resp, err := websocket.Dial(ctx, wsURL, nil)
		cancel()
		if err == nil {
			t.Fatalf("ws dial with %s token succeeded", name)
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("ws dial with %s token: status = %v, want 401", name, resp)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// The legacy server (no Deps) is unaffected: no tokens anywhere.
	legacy := newTestBackend(t, 21)
	createdLegacy := createMatch(t, legacy.http.URL, 1)
	_ = createdLegacy
	if s := getStateStatus(legacy.http.URL, createdLegacy.Match.ID, "anyone", ""); s != http.StatusOK {
		t.Fatalf("legacy private state status = %d, want 200", s)
	}
	status, _, _ = postJSON(legacy.http.URL+"/v1/players", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("legacy POST players status = %d, want 503", status)
	}
	status, _, _ = postJSON(legacy.http.URL+"/v1/matchmaking/join", map[string]any{})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("legacy matchmaking status = %d, want 503", status)
	}
	resp, err := http.Get(legacy.http.URL + "/v1/presence")
	if err != nil {
		t.Fatalf("legacy presence: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("legacy presence status = %d, want 503", resp.StatusCode)
	}
}

// TestB6MatchmakingFullGameReconnect is the B6 integration gate: two
// anonymous clients pair through the matchmaking queue, play a full game over
// real WebSockets, drop a socket mid-game, recover through token-authenticated
// GET state, reconnect with the same identity, and finish. Presence tracks the
// sockets throughout.
func TestB6MatchmakingFullGameReconnect(t *testing.T) {
	b := newTestB6Backend(t, 22)
	alice := createPlayer(t, b.http.URL)
	bob := createPlayer(t, b.http.URL)

	aliceJoin := joinAsync(b.http.URL, joinRequest(alice, 1))
	waitForQueueDepth(t, b.http.URL, 1)
	if n := getMatchmakingStatus(t, b.http.URL); n != 1 {
		t.Fatalf("matchmaking status = %d, want 1", n)
	}
	bobJoin := joinAsync(b.http.URL, joinRequest(bob, 1))

	aOut, bOut := <-aliceJoin, <-bobJoin
	if aOut.err != nil || bOut.err != nil {
		t.Fatalf("matchmaking errors: %v / %v", aOut.err, bOut.err)
	}
	if aOut.status != http.StatusOK || bOut.status != http.StatusOK {
		t.Fatalf("matchmaking statuses: %d / %d", aOut.status, bOut.status)
	}
	if aOut.resp.MatchID == "" || aOut.resp.MatchID != bOut.resp.MatchID {
		t.Fatalf("paired matches differ: %+v vs %+v", aOut.resp, bOut.resp)
	}
	if aOut.resp.Seat != 0 || bOut.resp.Seat != 1 {
		t.Fatalf("paired seats = %d / %d, want 0 and 1", aOut.resp.Seat, bOut.resp.Seat)
	}
	waitForQueueDepth(t, b.http.URL, 0)
	matchID := aOut.resp.MatchID

	aliceConn := dialPlayerWithToken(t, b.http.URL, matchID, alice)
	bobConn := dialPlayerWithToken(t, b.http.URL, matchID, bob)

	aliceState := readStateAtLeast(t, aliceConn, 3)
	bobState := readStateAtLeast(t, bobConn, 3)
	assertPrivateState(t, aliceState.state, 0)
	assertPrivateState(t, bobState.state, 1)

	// Both sockets are live: presence sees two players.
	waitForPresence(t, b.http.URL, 2)
	if !getPlayerOnline(t, b.http.URL, alice.id) || !getPlayerOnline(t, b.http.URL, bob.id) {
		t.Fatal("paired players not reported online")
	}

	clients := map[int]*websocket.Conn{0: aliceConn, 1: bobConn}
	states := map[int]observedState{0: aliceState, 1: bobState}
	moveNumber := 0
	reconnected := false

	for states[0].state.Status != "finished" {
		if moveNumber > 5000 {
			t.Fatal("match did not finish in 5000 moves")
		}
		turn := states[0].state.Turn
		current := states[turn]
		payload, ok := chooseWireMove(current.state, fmt.Sprintf("b6m%d", moveNumber))
		if !ok {
			t.Fatalf("seat %d has no legal move at seq %d", turn, current.seq)
		}
		writeMessage(t, clients[turn], Envelope{Type: "move", Seq: current.seq, Payload: payload})
		nextSeq := current.seq + 1
		states[0] = readStateAtLeast(t, clients[0], nextSeq)
		states[1] = readStateAtLeast(t, clients[1], nextSeq)
		moveNumber++

		if !reconnected && moveNumber == 3 && states[0].state.Status != "finished" {
			beforeDrop := states[0]
			_ = aliceConn.CloseNow() // abrupt mobile-network-style drop

			bobAfterDrop := readStateAtLeast(t, bobConn, beforeDrop.seq+1)
			states[1] = bobAfterDrop
			waitForPresence(t, b.http.URL, 1)
			if getPlayerOnline(t, b.http.URL, alice.id) {
				t.Fatal("dropped player still reported online")
			}

			// Identity survives the dead socket: the token alone recovers
			// the authoritative private state.
			recovered := getStateWithToken(t, b.http.URL, matchID, alice)
			if recovered.Seq != bobAfterDrop.seq {
				t.Fatalf("GET recovery seq = %d, broadcast seq = %d", recovered.Seq, bobAfterDrop.seq)
			}
			if len(recovered.State.Hand) == 0 || playerPresent(recovered.State, alice.id) {
				t.Fatal("recovered state missing hand or still present")
			}

			aliceConn = dialPlayerWithToken(t, b.http.URL, matchID, alice)
			clients[0] = aliceConn
			states[0] = readStateAtLeast(t, aliceConn, recovered.Seq+1)
			states[1] = readStateAtLeast(t, bobConn, recovered.Seq+1)
			if states[0].state.Viewer == nil || *states[0].state.Viewer != 0 {
				t.Fatalf("reconnected alice has viewer %v, want seat 0", states[0].state.Viewer)
			}
			if !playerPresent(states[0].state, alice.id) {
				t.Fatal("alice not marked present after reconnect")
			}
			waitForPresence(t, b.http.URL, 2)
			reconnected = true
		}
	}

	if !reconnected {
		t.Fatal("gate coverage: mid-game reconnect never ran")
	}
	if states[0].state.Winner == nil || states[1].state.Winner == nil {
		t.Fatal("finished state has no winner")
	}
	if *states[0].state.Winner != *states[1].state.Winner {
		t.Fatalf("clients disagree on winner: %d vs %d", *states[0].state.Winner, *states[1].state.Winner)
	}
	t.Logf("winner=%d moves=%d final_seq=%d", *states[0].state.Winner, moveNumber, states[0].seq)

	_ = aliceConn.CloseNow()
	_ = bobConn.CloseNow()
	waitForPresence(t, b.http.URL, 0)
}

// TestB6MatchmakingLeaveAndValidation covers the queue's control plane:
// explicit leave, auth on the lobby routes, and option validation.
func TestB6MatchmakingLeaveAndValidation(t *testing.T) {
	b := newTestB6Backend(t, 23)
	alice := createPlayer(t, b.http.URL)

	pending := joinAsync(b.http.URL, joinRequest(alice, 1))
	waitForQueueDepth(t, b.http.URL, 1)

	// A forged token cannot dequeue someone else's (or anyone's) wait.
	status, _, _ := postJSON(b.http.URL+"/v1/matchmaking/leave",
		leaveMatchmakingRequest{PlayerID: alice.id, Token: "forged"})
	if status != http.StatusUnauthorized {
		t.Fatalf("forged leave status = %d, want 401", status)
	}
	waitForQueueDepth(t, b.http.URL, 1)

	// An explicit leave ends the wait: the control call reports the removal
	// and the still-open long-poll ends with 204, not an error.
	status, data, err := postJSON(b.http.URL+"/v1/matchmaking/leave",
		leaveMatchmakingRequest{PlayerID: alice.id, Token: alice.token})
	if err != nil || status != http.StatusOK {
		t.Fatalf("leave status = %d, %v: %s", status, err, data)
	}
	var left leaveMatchmakingResponse
	if err := json.Unmarshal(data, &left); err != nil || !left.Cancelled {
		t.Fatalf("leave response = %+v, %v; want cancelled=true", left, err)
	}
	if out := <-pending; out.status != http.StatusNoContent {
		t.Fatalf("long-poll after leave status = %d, want 204", out.status)
	}
	waitForQueueDepth(t, b.http.URL, 0)

	// Leaving again is a no-op, not an error.
	status, data, _ = postJSON(b.http.URL+"/v1/matchmaking/leave",
		leaveMatchmakingRequest{PlayerID: alice.id, Token: alice.token})
	var leftAgain leaveMatchmakingResponse
	if err := json.Unmarshal(data, &leftAgain); err != nil || status != http.StatusOK || leftAgain.Cancelled {
		t.Fatalf("second leave = %d %+v; want 200 cancelled=false", status, leftAgain)
	}

	// Option validation happens before queueing.
	out := postMatchmakingJoin(b.http.URL, joinMatchmakingRequest{
		PlayerID: alice.id, Token: alice.token, SequencesToWin: -1,
	})
	if out.status != http.StatusUnprocessableEntity {
		t.Fatalf("negative sequences_to_win status = %d, want 422", out.status)
	}
	waitForQueueDepth(t, b.http.URL, 0)

	// Malformed JSON is a 400.
	status, _, _ = postJSON(b.http.URL+"/v1/matchmaking/join", "not-an-object")
	if status != http.StatusBadRequest {
		t.Fatalf("malformed join status = %d, want 400", status)
	}
}
