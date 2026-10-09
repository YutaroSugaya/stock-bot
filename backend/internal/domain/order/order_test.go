package order

import "testing"

func TestSide_Opposite(t *testing.T) {
	cases := []struct {
		name string
		in   Side
		want Side
	}{
		{"buy_to_sell", SideBuy, SideSell},
		{"sell_to_buy", SideSell, SideBuy},
		{"empty_stays_empty", Side(""), Side("")},
		{"junk_returns_empty", Side("FOO"), Side("")},
		{"lowercase_returns_empty", Side("buy"), Side("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Opposite(); got != tc.want {
				t.Fatalf("Opposite(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSide_Valid(t *testing.T) {
	cases := []struct {
		in   Side
		want bool
	}{
		{SideBuy, true}, {SideSell, true}, {Side(""), false}, {Side("FOO"), false},
	}
	for _, tc := range cases {
		if got := tc.in.Valid(); got != tc.want {
			t.Fatalf("Valid(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
