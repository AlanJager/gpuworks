package planning

// CacheRetryBlocked refuses a cache copy retry for a reason the operator can
// act on (connect the node, free cache space, wait for the old copy to exit).
type CacheRetryBlocked struct{ Detail string }

func (e *CacheRetryBlocked) Error() string { return e.Detail }
