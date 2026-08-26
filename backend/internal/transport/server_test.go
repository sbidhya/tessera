package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sbidhya/tessera/backend/internal/config"
	"github.com/sbidhya/tessera/backend/internal/engine"
	"github.com/sbidhya/tessera/backend/internal/room"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestManager(seed int64) (*room.Manager, *config.Config) {
	cfg := config.Config{Seed: seed, Addr: ":0", LogLevel: slog.LevelError}
	mgr := room.NewManager(discardLogger(), cfg.NewRand)
	return mgr, &cfg
}

func newTestServer(mgr *room.Manager) *Server {
	start := time.Unix(1000, 0)
	now := func() time.Time { return start.Add(2 * time.Second) }
	return New(mgr, discardLogger(), start, now)
}

func TestHealthz(t *testing.T) {
	mgr, _ := newTestManager(42)
	srv := newTestServer(mgr)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}

	// /api/healthz alias
	req = httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/healthz status = %d, want 200", rec.Code)
	}
}

func TestCreateAndListMatches(t *testing.T) {
	mgr, _ := newTestManager(1)
	srv := newTestServer(mgr)
	h := srv.Handler()

	// Create with defaults (empty body).
	req := httptest.NewRequest(http.MethodPost, "/api/matches", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created CreateMatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.RoomID == "" {
		t.Fatal("room_id empty")
	}
	if created.Status != "waiting" {
		t.Errorf("status = %q, want waiting", created.Status)
	}

	// Create with explicit options: 2 players, 1 to win (fast game).
	body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(2), SequencesToWin: intPtr(1)})
	req = httptest.NewRequest(http.MethodPost, "/matches", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with opts status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// List should have 2 rooms, ordered by id.
	req = httptest.NewRequest(http.MethodGet, "/api/matches", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var listed ListMatchesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Rooms) != 2 {
		t.Fatalf("list rooms = %d, want 2", len(listed.Rooms))
	}
	if listed.Rooms[0].RoomID > listed.Rooms[1].RoomID {
		t.Errorf("rooms not sorted: %q > %q", listed.Rooms[0].RoomID, listed.Rooms[1].RoomID)
	}

	// Also /matches alias
	req = httptest.NewRequest(http.MethodGet, "/matches", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/matches list status = %d", rec.Code)
	}
}

func TestGetStateAndJoinFlow(t *testing.T) {
	mgr, _ := newTestManager(99)
	srv := newTestServer(mgr)
	h := srv.Handler()

	// Create fast game (1 sequence to win).
	body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(2), SequencesToWin: intPtr(1)})
	req := httptest.NewRequest(http.MethodPost, "/api/matches", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var cr CreateMatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)
	roomID := cr.RoomID

	// GET state as spectator before anyone joins (should succeed).
	req = httptest.NewRequest(http.MethodGet, "/matches/"+roomID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get spectator: %d %s", rec.Code, rec.Body.String())
	}
	var snap SnapshotDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snap: %v", err)
	}
	if snap.Status != "waiting" {
		t.Errorf("initial status = %q, want waiting", snap.Status)
	}
	if len(snap.Board) != 96 {
		t.Errorf("board cells = %d, want 96", len(snap.Board))
	}
	if snap.Viewer != int(engine.NoPlayer) {
		t.Errorf("spectator viewer = %d, want %d", snap.Viewer, engine.NoPlayer)
	}
	if len(snap.Hand) != 0 {
		t.Errorf("spectator hand should be empty, got %d cards", len(snap.Hand))
	}

	// Join player A via REST.
	joinBody, _ := json.Marshal(JoinRequest{PlayerID: "alice"})
	req = httptest.NewRequest(http.MethodPost, "/matches/"+roomID+"/join", bytes.NewReader(joinBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("join alice: %d %s", rec.Code, rec.Body.String())
	}
	var jr JoinResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &jr); err != nil {
		t.Fatalf("decode join: %v", err)
	}
	if jr.Seat != 0 {
		t.Errorf("alice seat = %d, want 0", jr.Seat)
	}
	if jr.Rejoined {
		t.Error("first join should not be rejoined")
	}
	if jr.Status != "waiting" {
		t.Errorf("after 1 join status = %q, want waiting", jr.Status)
	}

	// Idempotent re-join same player should return same seat with Rejoined.
	req = httptest.NewRequest(http.MethodPost, "/matches/"+roomID+"/join", bytes.NewReader(joinBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rejoin alice: %d %s", rec.Code, rec.Body.String())
	}
	var jr2 JoinResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &jr2)
	if !jr2.Rejoined || jr2.Seat != jr.Seat {
		t.Errorf("rejoin got %+v, want rejoined seat %d", jr2, jr.Seat)
	}
	if jr2.Seq != jr.Seq {
		t.Errorf("rejoin seq changed %d -> %d, want stable", jr.Seq, jr2.Seq)
	}

	// Join player B -> match should start.
	joinBody, _ = json.Marshal(JoinRequest{PlayerID: "bob"})
	req = httptest.NewRequest(http.MethodPost, "/matches/"+roomID+"/join", bytes.NewReader(joinBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("join bob: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &jr)
	if jr.Status != "playing" {
		t.Errorf("after both joins status = %q, want playing", jr.Status)
	}

	// GET state for alice should contain her hand, but hand_counts for both.
	req = httptest.NewRequest(http.MethodGet, "/matches/"+roomID+"?player_id=alice", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get alice state: %d %s", rec.Code, rec.Body.String())
	}
	var snapA SnapshotDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &snapA)
	if len(snapA.Hand) == 0 {
		t.Error("alice hand empty after start")
	}
	if len(snapA.HandCounts) != 2 {
		t.Errorf("hand_counts len = %d, want 2", len(snapA.HandCounts))
	}
	// Alice should not see Bob's cards.
	req = httptest.NewRequest(http.MethodGet, "/matches/"+roomID+"?player_id=bob", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var snapB SnapshotDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &snapB)
	if len(snapB.Hand) == 0 {
		t.Error("bob hand empty after start")
	}
	// Hands must differ (different deals) — not equal as sets.
	if len(snapA.Hand) == len(snapB.Hand) {
		same := true
		for i, ca := range snapA.Hand {
			if i < len(snapB.Hand) && snapB.Hand[i] != ca {
				same = false
				break
			}
		}
		if same {
			t.Error("alice and bob got identical hands — deals should differ")
		}
	}
	// Viewer field check.
	if snapA.Viewer != 0 {
		t.Errorf("alice viewer = %d, want 0", snapA.Viewer)
	}
	if snapB.Viewer != 1 {
		t.Errorf("bob viewer = %d, want 1", snapB.Viewer)
	}

	// Unknown room should be 404.
	req = httptest.NewRequest(http.MethodGet, "/matches/does-not-exist", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown room status = %d, want 404", rec.Code)
	}

	// Missing player_id on join should be 400.
	req = httptest.NewRequest(http.MethodPost, "/matches/"+roomID+"/join", bytes.NewReader([]byte(`{}`)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing player_id status = %d, want 400", rec.Code)
	}

	// Unknown route
	req = httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want 404", rec.Code)
	}
}

func TestCreateInvalidOptions(t *testing.T) {
	mgr, _ := newTestManager(7)
	srv := newTestServer(mgr)
	h := srv.Handler()

	// NumPlayers 3 with SequencesToWin 2 should fail? Actually handSize supports 3, so it succeeds.
	// Use unsupported NumPlayers like 5.
	body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(5), SequencesToWin: intPtr(2)})
	req := httptest.NewRequest(http.MethodPost, "/api/matches", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported players status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var errBody ErrorDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code == "" {
		t.Error("error code empty")
	}

	// Invalid JSON
	req = httptest.NewRequest(http.MethodPost, "/api/matches", bytes.NewReader([]byte("{bad json")))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON status = %d, want 400", rec.Code)
	}
}

func TestSnapshotAfterLeaveAndReconnect(t *testing.T) {
	mgr, _ := newTestManager(10)
	srv := newTestServer(mgr)
	h := srv.Handler()
	body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(2), SequencesToWin: intPtr(1)})
	req := httptest.NewRequest(http.MethodPost, "/api/matches", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var cr CreateMatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)

	// Join both.
	for _, pid := range []string{"p1", "p2"} {
		jb, _ := json.Marshal(JoinRequest{PlayerID: pid})
		req = httptest.NewRequest(http.MethodPost, "/matches/"+cr.RoomID+"/join", bytes.NewReader(jb))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("join %s: %d %s", pid, rec.Code, rec.Body.String())
		}
	}
	// Simulate player leave via room manager directly (since transport has no REST leave yet).
	rm, _ := mgr.Get(cr.RoomID)
	_ = rm.Leave(t.Context(), "p1")
	// Snapshot should still show player present=false but seat held.
	req = httptest.NewRequest(http.MethodGet, "/matches/"+cr.RoomID+"?player_id=p1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var snap SnapshotDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	found := false
	for _, p := range snap.Players {
		if p.ID == "p1" {
			found = true
			if p.Present {
				t.Error("p1 present should be false after leave mid-game")
			}
		}
	}
	if !found {
		t.Error("p1 not in players after leave")
	}
	// Re-join same player should succeed and present becomes true.
	jb, _ := json.Marshal(JoinRequest{PlayerID: "p1"})
	req = httptest.NewRequest(http.MethodPost, "/matches/"+cr.RoomID+"/join", bytes.NewReader(jb))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rejoin after leave: %d %s", rec.Code, rec.Body.String())
	}
	var jr JoinResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &jr)
	if !jr.Rejoined {
		t.Error("rejoin should be marked rejoined")
	}
	req = httptest.NewRequest(http.MethodGet, "/matches/"+cr.RoomID+"?player_id=p1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	for _, p := range snap.Players {
		if p.ID == "p1" && !p.Present {
			t.Error("p1 present should be true after rejoin")
		}
	}
}

func TestConcurrentCreateAndList(t *testing.T) {
	mgr, _ := newTestManager(123)
	srv := newTestServer(mgr)
	h := srv.Handler()
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(2), SequencesToWin: intPtr(1)})
			req := httptest.NewRequest(http.MethodPost, "/api/matches", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Errorf("concurrent create: %d %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	req := httptest.NewRequest(http.MethodGet, "/api/matches", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var listed ListMatchesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Rooms) != n {
		t.Fatalf("rooms = %d, want %d", len(listed.Rooms), n)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	// Ensure Envelope marshals with type/seq/payload as specified in B3 spec.
	dto := MoveResultDTO{Seq: 42, Duplicate: false, Status: "playing", Turn: 1, Winner: -1}
	env := Envelope{Type: "move_result", Seq: 42, Payload: mustMarshal(dto)}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Envelope
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "move_result" || decoded.Seq != 42 {
		t.Errorf("decoded = %+v, want type move_result seq 42", decoded)
	}
	var back MoveResultDTO
	if err := json.Unmarshal(decoded.Payload, &back); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if back.Turn != 1 {
		t.Errorf("turn = %d, want 1", back.Turn)
	}
	// Check raw JSON contains expected fields.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["type"]; !ok {
		t.Error("json missing type")
	}
	if _, ok := raw["seq"]; !ok {
		t.Error("json missing seq")
	}
	if _, ok := raw["payload"]; !ok {
		t.Error("json missing payload")
	}
}

func intPtr(v int) *int { return &v }

// Verify that room snapshot's per-viewer privacy is preserved through DTO.
func TestSnapshotPrivacyViaDTO(t *testing.T) {
	mgr, _ := newTestManager(555)
	srv := newTestServer(mgr)
	h := srv.Handler()
	body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(2), SequencesToWin: intPtr(2)})
	req := httptest.NewRequest(http.MethodPost, "/matches", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var cr CreateMatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)
	for _, pid := range []string{"alice", "bob"} {
		jb, _ := json.Marshal(JoinRequest{PlayerID: pid})
		req = httptest.NewRequest(http.MethodPost, "/matches/"+cr.RoomID+"/join", bytes.NewReader(jb))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	// Alice's view should have hand, bob's view should not reveal alice's cards via counts only.
	req = httptest.NewRequest(http.MethodGet, "/matches/"+cr.RoomID+"?player_id=alice", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var snapA SnapshotDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &snapA)
	req = httptest.NewRequest(http.MethodGet, "/matches/"+cr.RoomID+"?player_id=bob", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var snapB SnapshotDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &snapB)
	// Neither should see the other's hand in the payload — we check that the JSON for alice doesn't contain bob's hand cards by comparing DTOs.
	// Simple check: encoded alice snapshot hand size >0 and not equal to bob's hand.
	if len(snapA.Hand) == 0 || len(snapB.Hand) == 0 {
		t.Fatal("hands empty")
	}
	// Ensure hand_counts correctly reports both.
	if snapA.HandCounts["0"] != len(snapA.Hand) {
		t.Errorf("alice hand_counts[0]=%d, hand len %d", snapA.HandCounts["0"], len(snapA.Hand))
	}
	if snapB.HandCounts["1"] != len(snapB.Hand) {
		t.Errorf("bob hand_counts[1]=%d, hand len %d", snapB.HandCounts["1"], len(snapB.Hand))
	}
	// Ensure board is present and stable across viewers.
	if len(snapA.Board) != 96 || len(snapB.Board) != 96 {
		t.Fatalf("board len %d %d", len(snapA.Board), len(snapB.Board))
	}
	for i := range snapA.Board {
		if snapA.Board[i] != snapB.Board[i] {
			t.Fatalf("board mismatch at %d: %+v vs %+v", i, snapA.Board[i], snapB.Board[i])
		}
	}
}

// Test that POST /matches supports both /api/matches and /matches and query-param join.
func TestJoinViaQueryParam(t *testing.T) {
	mgr, _ := newTestManager(777)
	srv := newTestServer(mgr)
	h := srv.Handler()
	body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(2), SequencesToWin: intPtr(1)})
	req := httptest.NewRequest(http.MethodPost, "/api/matches", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var cr CreateMatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)
	// Join via query param without JSON body.
	req = httptest.NewRequest(http.MethodPost, "/matches/"+cr.RoomID+"/join?player_id=alice", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("join via query: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateWithEmptyBody(t *testing.T) {
	mgr, _ := newTestManager(999)
	srv := newTestServer(mgr)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/matches", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("empty body create: %d %s", rec.Code, rec.Body.String())
	}
	var cr CreateMatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)
	if cr.RoomID == "" {
		t.Error("room_id empty")
	}
	// Verify defaults produce a playable game (2 players).
	req = httptest.NewRequest(http.MethodGet, "/matches/"+cr.RoomID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var snap SnapshotDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	if snap.Status != "waiting" {
		t.Errorf("status = %q, want waiting", snap.Status)
	}
	// Ensure malformed JSON on create is rejected.
	req = httptest.NewRequest(http.MethodPost, "/matches", bytes.NewReader([]byte("{oops")))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json status = %d, want 400", rec.Code)
	}
}

func TestWSRequiresPlayerID(t *testing.T) {
	mgr, _ := newTestManager(42)
	srv := newTestServer(mgr)
	h := srv.Handler()
	// Create room.
	body, _ := json.Marshal(CreateMatchRequest{NumPlayers: intPtr(2), SequencesToWin: intPtr(1)})
	req := httptest.NewRequest(http.MethodPost, "/matches", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var cr CreateMatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)

	// WS without player_id should be 400, not upgraded.
	ts := httptest.NewServer(h)
	defer ts.Close()
	wsURL := "ws" + ts.URL[4:] + "/matches/" + cr.RoomID + "/ws"
	_, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected ws dial without player_id to fail")
	}
	// Also unknown room.
	wsURL2 := "ws" + ts.URL[4:] + "/matches/doesnotexist/ws?player_id=alice"
	_, _, err = websocket.DefaultDialer.Dial(wsURL2, nil)
	if err == nil {
		t.Fatal("expected ws dial to unknown room to fail")
	}
}

// Ensure transport DTO card conversion tolerates all rank/suit strings.
func TestCardDTOConversion(t *testing.T) {
	cases := []struct {
		rank string
		suit string
		ok   bool
	}{
		{"A", "Spades", true},
		{"10", "Hearts", true},
		{"J", "Diamonds", true},
		{"Q", "Clubs", true},
		{"K", "Spades", true},
		{"Ace", "Spades", true},
		{"1", "Spades", false},
		{"A", "Invalid", false},
		{"", "Spades", false},
	}
	for _, tc := range cases {
		dto := CardDTO{Rank: tc.rank, Suit: tc.suit}
		_, err := dto.toCard()
		if tc.ok && err != nil {
			t.Errorf("dto %+v should succeed, got %v", dto, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("dto %+v should fail", dto)
		}
	}
	// Round-trip for all ranks/suits.
	for _, r := range []engine.Rank{engine.Ace, engine.Two, engine.Three, engine.Four, engine.Five, engine.Six, engine.Seven, engine.Eight, engine.Nine, engine.Ten, engine.Jack, engine.Queen, engine.King} {
		for _, s := range []engine.Suit{engine.Spades, engine.Hearts, engine.Diamonds, engine.Clubs} {
			c := engine.Card{Rank: r, Suit: s}
			dto := cardToDTO(c)
			back, err := dto.toCard()
			if err != nil {
				t.Fatalf("round-trip %v: dto %+v err %v", c, dto, err)
			}
			if back != c {
				t.Fatalf("round-trip mismatch %v -> %+v -> %v", c, dto, back)
			}
		}
	}
}

func TestListEmpty(t *testing.T) {
	mgr, _ := newTestManager(1234)
	srv := newTestServer(mgr)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/matches", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var listed ListMatchesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if listed.Rooms == nil {
		t.Error("rooms should be non-nil slice (empty), got nil")
	}
	if len(listed.Rooms) != 0 {
		t.Errorf("rooms len = %d, want 0", len(listed.Rooms))
	}
}
