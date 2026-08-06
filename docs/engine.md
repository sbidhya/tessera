# B1 — Game engine design notes

The engine (`backend/internal/engine`) is the pure, I/O-free core: the rules of
Sequence and nothing else. It imports only the standard library, so the
inward-pointing layering (engine ← room ← transport ← persistence ← infra) can
never be violated by an accidental import of networking or a database.

## Why "pure" matters here

"Pure" means: no I/O, no hidden global state, and no randomness that isn't
injected. Every source of randomness (deck shuffle, board layout) comes from a
`*rand.Rand` passed in by the caller. Given the same injected RNG and the same
sequence of moves, the engine produces **byte-identical state on every run**.

That single property pays off repeatedly:

- Tests are deterministic and reproducible from one integer seed (project
  principle #4).
- The durability layer (B4) can replay a write-ahead log of accepted moves to
  rebuild an identical `GameState` after a crash.
- Bugs are reproducible: a seed + a move list fully reconstructs any position.

## Transactional moves

`GameState.Apply(Move)` validates a move **completely before mutating anything**.
A rejected move (out of turn, illegal target, duplicate dead-card swap, …)
returns a typed sentinel error and leaves the state untouched. The room manager
(B2) leans on this for idempotent, out-of-turn-rejecting command handling: it can
try to apply a move and know that failure has no side effects.

## Why we generate the board instead of hardcoding it

Physical Sequence ships a fixed 10×10 board. We instead **generate the layout
deterministically from the injected RNG**, guaranteeing by construction that each
of the 48 non-jack cards lands on exactly two of the 96 non-corner cells, with
the four corners wild.

Reasoning:

- **Correct by construction.** A hand-transcribed 100-cell table is exactly the
  kind of silent data bug that would poison the crown-jewel engine (a card
  appearing three times, another once). Generation makes the invariant a
  property of the code, checked by `TestBoardInvariants`.
- **Reproducible.** Same seed → same board, so the whole game still replays from
  one integer.
- **Swappable.** The rules depend only on the `Board` value (the cell↔card map
  and corner geometry), not on how it was produced. Dropping in the canonical
  physical layout later is a change to `NewBoard` alone, with zero rule changes.

Trade-off: the board differs from the physical game's fixed layout, so strategy
that relies on memorizing the real board doesn't transfer. That's acceptable for
v1 and reversible; noted here as a deliberate default.

## Sequence detection & the overlap rule

When a chip is placed, we scan the four line orientations (horizontal, vertical,
both diagonals) through the placed cell for a run of five cells that all count for
the player — a chip they own, or a wild corner (corners count for everyone).

Completed sequences lock their chips (`Chip.InSequence`), which makes them immune
to one-eyed-jack removal. A newly formed run counts as a *new* sequence only if it
shares **at most one** already-locked non-corner cell with the player's existing
sequences — the standard "you may reuse one chip" rule. Corners are wild and
shared freely, so they're exempt from the shared-cell limit.

A single placement at the intersection of two ready lines can complete two
sequences at once; `TestDetectTwoSequencesAtOnce` covers that.

## Scope (v1)

- 2 players; `SequencesToWin` configurable (default 2, use 1 for fast games).
- Empty draw pile is a rare late-game edge case: a play still succeeds but no
  replacement is drawn (the hand shrinks).
