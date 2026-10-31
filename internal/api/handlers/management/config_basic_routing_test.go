package management

import "testing"

func TestNormalizeRoutingStrategyAcceptsSoonestReset(t *testing.T) {
	for _, raw := range []string{"soonest-reset", "soonestreset", "SR"} {
		got, ok := normalizeRoutingStrategy(raw)
		if !ok || got != "soonest-reset" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want soonest-reset, true", raw, got, ok)
		}
	}
}
