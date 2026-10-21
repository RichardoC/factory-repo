package ttlcache

import "runtime"

// testGoroutines is how many goroutines exist right now.
func testGoroutines() int { return runtime.NumGoroutine() }
