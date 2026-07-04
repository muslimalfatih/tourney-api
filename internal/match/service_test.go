package match

import "testing"

func ptr(i int) *int { return &i }

func TestDecideWinner(t *testing.T) {
	cases := []struct {
		name string
		sets []SetScore
		want int // 1, 2, or 0 (undecided)
	}{
		{
			name: "straight sets to p1",
			sets: []SetScore{{P1Games: 6, P2Games: 3}, {P1Games: 6, P2Games: 4}},
			want: 1,
		},
		{
			name: "straight sets to p2",
			sets: []SetScore{{P1Games: 2, P2Games: 6}, {P1Games: 4, P2Games: 6}},
			want: 2,
		},
		{
			name: "three sets, p1 wins the decider",
			sets: []SetScore{{P1Games: 6, P2Games: 4}, {P1Games: 3, P2Games: 6}, {P1Games: 7, P2Games: 5}},
			want: 1,
		},
		{
			name: "tiebreak decides an equal-games set",
			sets: []SetScore{{P1Games: 7, P2Games: 6, P1Tiebreak: ptr(7), P2Tiebreak: ptr(5)}, {P1Games: 6, P2Games: 2}},
			want: 1,
		},
		{
			name: "tiebreak the other way",
			sets: []SetScore{{P1Games: 6, P2Games: 7, P1Tiebreak: ptr(4), P2Tiebreak: ptr(7)}, {P1Games: 3, P2Games: 6}},
			want: 2,
		},
		{
			name: "one set each is undecided",
			sets: []SetScore{{P1Games: 6, P2Games: 3}, {P1Games: 2, P2Games: 6}},
			want: 0,
		},
		{
			name: "equal games without tiebreak counts for neither",
			sets: []SetScore{{P1Games: 6, P2Games: 6}},
			want: 0,
		},
		{
			name: "single set winner",
			sets: []SetScore{{P1Games: 6, P2Games: 0}},
			want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideWinner(c.sets); got != c.want {
				t.Errorf("decideWinner() = %d, want %d", got, c.want)
			}
		})
	}
}
