package runner

import (
	"io"
	"math"
)

// Estimates and usage counters must never wrap into a small or negative quota.
func addSize(a, b int64) int64 {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func multiplySize(a, b int64) int64 {
	if a < 0 || b < 0 || a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}

// A zero task quota leaves the reader bounded only by its source.
func quotaReader(r io.Reader, limit int64) io.Reader {
	if limit > 0 {
		return io.LimitReader(r, limit)
	}
	return r
}

func tokenBudget(spec RunSpec) int64 {
	budget := int64(spec.Limits.MaxTokens)
	if spec.DailyTokenBudget > 0 && (budget == 0 || spec.DailyTokenBudget < budget) {
		budget = spec.DailyTokenBudget
	}
	return budget
}
