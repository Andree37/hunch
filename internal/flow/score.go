package flow

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MatchScore reports whether a score value satisfies a branch condition:
// ">=4", ">3", "<=2", "<3", "2-4" (inclusive), or "3" (value rounds to 3).
func MatchScore(when string, v float64) (bool, error) {
	w := strings.TrimSpace(when)
	for _, op := range []string{">=", "<=", ">", "<"} {
		if rest, ok := strings.CutPrefix(w, op); ok {
			x, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return false, badScore(when)
			}
			switch op {
			case ">=":
				return v >= x, nil
			case "<=":
				return v <= x, nil
			case ">":
				return v > x, nil
			default:
				return v < x, nil
			}
		}
	}
	if lo, hi, ok := strings.Cut(w, "-"); ok {
		a, err1 := strconv.ParseFloat(strings.TrimSpace(lo), 64)
		b, err2 := strconv.ParseFloat(strings.TrimSpace(hi), 64)
		if err1 != nil || err2 != nil || a > b {
			return false, badScore(when)
		}
		return v >= a && v <= b, nil
	}
	x, err := strconv.ParseFloat(w, 64)
	if err != nil {
		return false, badScore(when)
	}
	return math.Round(v) == x, nil
}

func badScore(when string) error {
	return fmt.Errorf("bad score condition %q (want e.g. >=4, <3, 2-4 or 3)", when)
}
