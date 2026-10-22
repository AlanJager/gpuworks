package app

func sameImageRefSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, ref := range left {
		leftSet[ref] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, ref := range right {
		rightSet[ref] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for ref := range leftSet {
		if _, ok := rightSet[ref]; !ok {
			return false
		}
	}
	return true
}
