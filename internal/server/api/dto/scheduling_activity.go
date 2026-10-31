package dto

import "time"

// SchedulingActivity is the scheduling activity over a range: the scheduling
// events per bucket, stacked by reason, and the cards that changed hands
// most (GET /scheduling/activity).
type SchedulingActivity struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// BucketSeconds is the length of a bucket, which the server picks for
	// the range.
	BucketSeconds int64 `json:"bucket_seconds"`
	// Reasons are every reason, in the order they stack, with the range's
	// count of each.
	Reasons []ActivityReason `json:"reasons"`
	// Buckets cover the range from From, one per BucketSeconds, oldest
	// first; Counts has a count per reason, in Reasons' order.
	Buckets []ActivityBucket `json:"buckets"`
	// TopCards are the cards that changed hands most over the range, most
	// first, at most 20.
	TopCards []ActivityCard `json:"top_cards"`
	// BeyondRetention says the range begins before the statistics are kept:
	// its oldest part has no figures.
	BeyondRetention bool `json:"beyond_retention"`
}

// ActivityReason is one reason and its count over the range.
type ActivityReason struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// ActivityBucket is one bucket's counts per reason.
type ActivityBucket struct {
	Start  time.Time `json:"start"`
	Counts []int64   `json:"counts"`
}

// ActivityCard is a card and how often it changed hands (a yield or a wake
// on it). Node is the node's display name, empty for a node no longer known.
type ActivityCard struct {
	NodeUUID string `json:"node_uuid"`
	Node     string `json:"node"`
	GPUIndex int    `json:"gpu_index"`
	Switches int64  `json:"switches"`
}
