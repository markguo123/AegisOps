// Package authstate provides process-local invalidation without depending on
// identity or resource authorization services.
package authstate

import "sync"

var state = struct {
	sync.Mutex
	version uint64
	changed chan struct{}
}{changed: make(chan struct{})}

func Revision() uint64         { state.Lock(); defer state.Unlock(); return state.version }
func Changes() <-chan struct{} { state.Lock(); defer state.Unlock(); return state.changed }
func Invalidate() {
	state.Lock()
	defer state.Unlock()
	state.version++
	close(state.changed)
	state.changed = make(chan struct{})
}
