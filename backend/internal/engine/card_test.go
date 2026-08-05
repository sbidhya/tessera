package engine

import "testing"

func TestCardString(t *testing.T) {
	cases := []struct {
		card Card
		want string
	}{
		{Card{SuitHearts, RankAce}, "AH"},
		{Card{SuitDiamonds, Rank10}, "10D"},
		{Card{SuitClubs, RankJack}, "JC"},
		{Card{SuitSpades, RankQueen}, "QS"},
		{Card{SuitHearts, RankKing}, "KH"},
		{Card{SuitDiamonds, Rank2}, "2D"},
	}
	for _, tc := range cases {
		if got := tc.card.String(); got != tc.want {
			t.Errorf("Card%v.String() = %q, want %q", tc.card, got, tc.want)
		}
	}
}

func TestJackClassification(t *testing.T) {
	tests := []struct {
		card    Card
		isJack  bool
		twoEyed bool
		oneEyed bool
	}{
		{Card{SuitDiamonds, RankJack}, true, true, false},
		{Card{SuitClubs, RankJack}, true, true, false},
		{Card{SuitHearts, RankJack}, true, false, true},
		{Card{SuitSpades, RankJack}, true, false, true},
		{Card{SuitHearts, RankAce}, false, false, false},
		{Card{SuitDiamonds, RankQueen}, false, false, false},
	}
	for _, tc := range tests {
		if got := IsJack(tc.card); got != tc.isJack {
			t.Errorf("%v IsJack = %v want %v", tc.card, got, tc.isJack)
		}
		if got := IsTwoEyedJack(tc.card); got != tc.twoEyed {
			t.Errorf("%v IsTwoEyedJack = %v want %v", tc.card, got, tc.twoEyed)
		}
		if got := IsOneEyedJack(tc.card); got != tc.oneEyed {
			t.Errorf("%v IsOneEyedJack = %v want %v", tc.card, got, tc.oneEyed)
		}
	}
}

func TestSuitString(t *testing.T) {
	if SuitHearts.String() != "H" || SuitDiamonds.String() != "D" || SuitClubs.String() != "C" || SuitSpades.String() != "S" {
		t.Error("suit strings incorrect")
	}
}

func TestRankString(t *testing.T) {
	cases := map[Rank]string{
		RankAce: "A", Rank2: "2", RankJack: "J", RankQueen: "Q", RankKing: "K",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("rank %d = %q want %q", r, got, want)
		}
	}
}
