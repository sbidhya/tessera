// Package auth issues and verifies lightweight anonymous credentials.
//
// B6 needs identity without accounts: a mobile client generates no password,
// and there is no user database. Instead, POST /v1/players mints a random,
// unguessable player id, and every credential is an HMAC tag over that id keyed
// by a server-side secret. Verification is stateless — no token table, nothing
// to persist — so a credential issued before a restart still verifies after it
// (as long as the secret is stable), and a seat held in the WAL under that
// player id is still reachable. That is what "identity survives reconnect"
// means here: the client keeps two strings, and the server can always check
// the second against the first.
//
// The token is NOT a session: it carries no expiry and no permissions. It
// proves "I am the player who was issued this id", which is exactly the claim
// the room layer needs to seat a socket. Anything stronger (expiry, scopes,
// revocation) is a later block's problem.
//
// This is a leaf package: standard library only, so any layer may use it
// without bending the inward-pointing layering.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
)

// ErrInvalidToken is returned when a credential is missing, malformed, or was
// not issued by this server (wrong secret or tampered bytes).
var ErrInvalidToken = errors.New("auth: invalid token")

// idBytes is the entropy per player id: 48 bits, the same strength as room
// ids. Knowing a player id is no longer a capability (the token is checked
// too), but unguessable ids still keep presence probes and seat spam cheap to
// reject.
const idBytes = 6

// Authenticator mints player ids and signs/verifies their tokens. The zero
// value is unusable; build one with New. It is safe for concurrent use: the id
// generator is the only mutable state and it is mutex-guarded because
// math/rand/v2.Rand is not goroutine-safe.
type Authenticator struct {
	secret []byte
	mu     sync.Mutex
	ids    *rand.Rand
}

// New builds an Authenticator. secret is the HMAC key: it must be stable
// across restarts for old tokens to keep verifying (main derives a
// seed-pinned dev default and honours TESSERA_AUTH_SECRET), and it must be
// unpredictable in production, since anyone holding it can mint arbitrary
// identities. ids supplies player-id entropy; pass cfg.NewRand("player-ids")
// so ids stay deterministic under a fixed seed yet independent from every
// other stream.
func New(secret []byte, ids *rand.Rand) *Authenticator {
	if ids == nil {
		ids = rand.New(rand.NewPCG(0, 0))
	}
	return &Authenticator{secret: secret, ids: ids}
}

// Issue mints a fresh anonymous identity and its credential. The two returned
// strings must be stored together by the client; either one alone is useless.
func (a *Authenticator) Issue() (playerID, token string) {
	a.mu.Lock()
	var b [idBytes]byte
	for i := range b {
		b[i] = byte(a.ids.UintN(256))
	}
	a.mu.Unlock()

	playerID = "p_" + hex.EncodeToString(b[:])
	return playerID, a.Sign(playerID)
}

// Sign returns the credential for an already-known player id. It is exposed
// (rather than kept inside Issue) so tests and future administrative tools can
// re-derive a token without minting a new identity.
func (a *Authenticator) Sign(playerID string) string {
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(playerID))
	return playerID + "." + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks that token is the credential issued for playerID. It returns
// nil on success and ErrInvalidToken otherwise, using a constant-time
// comparison so token validity is not oracle-able byte by byte.
func (a *Authenticator) Verify(playerID, token string) error {
	id, sig, ok := strings.Cut(token, ".")
	if !ok || id == "" || sig == "" {
		return ErrInvalidToken
	}
	// The token must name the player it is presented for: a valid token for
	// player A presented as player B is a different claim and must fail, even
	// though both halves are individually well-formed.
	if id != playerID {
		return ErrInvalidToken
	}
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(playerID))
	expected := mac.Sum(nil)
	presented, err := hex.DecodeString(sig)
	if err != nil {
		return ErrInvalidToken
	}
	if !hmac.Equal(presented, expected) {
		return ErrInvalidToken
	}
	return nil
}
