package builtin

import "testing"

func TestNormalizeLimit(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{nil, 20},
		{float64(5), 5},
		{int(50), 50},
		{"30", 30},
		{0, 20},
		{-3, 20},
		{float64(500), 100},
		{"notanumber", 20},
	}
	for _, c := range cases {
		if got := normalizeLimit(c.in); got != c.want {
			t.Errorf("normalizeLimit(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
