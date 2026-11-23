package usage

import "testing"

func TestQuotaAllows(t *testing.T) {
	cases := []struct {
		used, limit, cost int64
		want              bool
	}{
		{0, 0, 100, true},    // disabled
		{0, 100, 1, true},    // fits
		{99, 100, 1, true},   // exactly at cap
		{99, 100, 2, false},  // would cross
		{100, 100, 1, false}, // already at cap
		{5, 10, -1, true},    // negative cost clamped
	}
	for _, c := range cases {
		if got := QuotaAllows(c.used, c.limit, c.cost); got != c.want {
			t.Fatalf("QuotaAllows(%d,%d,%d)=%v want %v", c.used, c.limit, c.cost, got, c.want)
		}
	}
}
