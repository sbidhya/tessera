package auth

import (
	"strings"
	"sync"
	"testing"

	"github.com/sbidhya/tessera/backend/internal/config"
)

func newTestAuth(secret string, seed int64) *Authenticator {
	cfg := config.Config{Seed: seed}
	return New([]byte(secret), cfg.NewRand("player-ids"))
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	a := newTestAuth("test-secret", 1)
	playerID, token := a.Issue()
	if !strings.HasPrefix(playerID, "p_") {
		t.Fatalf("player id = %q, want p_ prefix", playerID)
	}
	if err := a.Verify(playerID, token); err != nil {
		t.Fatalf("verify own token: %v", err)
	}
}

func TestIssuedIDsAreUnique(t *testing.T) {
	a := newTestAuth("test-secret", 1)
	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		id, _ := a.Issue()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate player id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestIssueIsDeterministicUnderSeed(t *testing.T) {
	a1 := newTestAuth("test-secret", 9)
	a2 := newTestAuth("test-secret", 9)
	id1, _ := a1.Issue()
	id2, _ := a2.Issue()
	if id1 != id2 {
		t.Fatalf("same seed issued %q vs %q", id1, id2)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	a := newTestAuth("test-secret", 1)
	id, token := a.Issue()
	// Flip the last hex digit of the signature.
	tampered := token[:len(token)-1] + "0"
	if tampered == token {
		tampered = token[:len(token)-1] + "1"
	}
	if err := a.Verify(id, tampered); err == nil {
		t.Fatal("tampered token verified")
	}
}

func TestWrongSecretRejected(t *testing.T) {
	a := newTestAuth("correct-secret", 1)
	other := newTestAuth("wrong-secret", 1)
	id, token := a.Issue()
	if err := other.Verify(id, token); err == nil {
		t.Fatal("token verified under the wrong secret")
	}
}

func TestTokenBoundToPlayerID(t *testing.T) {
	a := newTestAuth("test-secret", 1)
	idA, _ := a.Issue()
	_, tokenB := a.Issue()
	// tokenB is valid for B; presenting it as A must fail.
	if err := a.Verify(idA, tokenB); err == nil {
		t.Fatal("B's token verified as A's identity")
	}
	// A bare player id is not a token.
	if err := a.Verify(idA, idA); err == nil {
		t.Fatal("player id verified as its own token")
	}
}

func TestMalformedTokensRejected(t *testing.T) {
	a := newTestAuth("test-secret", 1)
	id, _ := a.Issue()
	for _, token := range []string{"", "no-separator", ".", id + ".", "." + id, id + ".zzzz"} {
		if err := a.Verify(id, token); err == nil {
			t.Errorf("malformed token %q verified", token)
		}
	}
}

func TestSignReproducesIssueToken(t *testing.T) {
	a := newTestAuth("test-secret", 1)
	id, token := a.Issue()
	if got := a.Sign(id); got != token {
		t.Fatalf("Sign(%q) = %q, Issue returned %q", id, got, token)
	}
}

// The id generator draws from a mutex-guarded stream; hammer it from many
// goroutines under -race to prove Issue is safe to call per HTTP request.
func TestConcurrentIssue(t *testing.T) {
	a := newTestAuth("test-secret", 1)
	const workers = 16
	const perWorker = 50
	ids := make(chan string, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id, token := a.Issue()
				if err := a.Verify(id, token); err != nil {
					t.Errorf("verify after concurrent issue: %v", err)
					return
				}
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]struct{})
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id under concurrency: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNilRandDoesNotPanic(t *testing.T) {
	a := New([]byte("s"), nil)
	id, token := a.Issue()
	if err := a.Verify(id, token); err != nil {
		t.Fatalf("verify with nil-rand authenticator: %v", err)
	}
}
