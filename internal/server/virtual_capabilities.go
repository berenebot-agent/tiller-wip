package server

// virtualTargetEligible is the one admin-side definition of a target that can
// currently receive traffic. virtualTargetView.Available intentionally omits
// the target's own enabled flag so that the admin UI can still explain why a
// disabled target is unavailable.
func virtualTargetEligible(target virtualTargetView) bool {
	return target.Enabled && target.Available
}

func eligibleVirtualTargets(targets []virtualTargetView) []virtualTargetView {
	eligible := make([]virtualTargetView, 0, len(targets))
	for _, target := range targets {
		if virtualTargetEligible(target) {
			eligible = append(eligible, target)
		}
	}
	return eligible
}

// aggregateVirtualNumeric returns the minimum safe positive value across the
// currently eligible targets. A missing or non-positive value on any eligible
// target makes the result unknown: advertising a larger limit could cause the
// request to fail on that target.
func aggregateVirtualNumeric(targets []virtualTargetView, value func(virtualTargetView) *int64) *int64 {
	eligible := eligibleVirtualTargets(targets)
	if len(eligible) == 0 {
		return nil
	}
	var minimum int64
	for i, target := range eligible {
		candidate := value(target)
		if candidate == nil || *candidate <= 0 {
			return nil
		}
		if i == 0 || *candidate < minimum {
			minimum = *candidate
		}
	}
	return &minimum
}
