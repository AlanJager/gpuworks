package store

import "sync"

// StartJoined starts n copies of fn and returns the group that tracks them.
func StartJoined(n int, fn func()) *sync.WaitGroup {
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	return &wg
}
