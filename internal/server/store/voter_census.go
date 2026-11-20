package store

// VoterCensus reports the voters a multi-member group can see.
// One member is the standalone server, not a voting group that lost its
// majority, so the census is not clustered and the count stays zero.
// Two or more members report the real voter count.
func VoterCensus(roles []string) (voters int, clustered bool) {
	if len(roles) < 2 {
		return 0, false
	}
	for _, role := range roles {
		if role == "voter" {
			voters++
		}
	}
	return voters, true
}
