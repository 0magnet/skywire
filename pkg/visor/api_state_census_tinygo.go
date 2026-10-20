//go:build tinygo

package visor

import (
	"runtime"
	"sync"
	"time"
)

var census struct {
	sync.Mutex
	at time.Time
	s  string
}

// heapCensus is an experiment: the TinyGo heap grouped by object shape, taken
// at most every ten minutes because it runs a collection and walks the heap.
func heapCensus() string {
	census.Lock()
	defer census.Unlock()
	if time.Since(census.at) >= 10*time.Minute {
		census.s = time.Now().Format(time.TimeOnly) + " " + runtime.HeapCensus(40)
		census.at = time.Now()
	}
	return census.s
}
