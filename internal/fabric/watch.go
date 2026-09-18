package fabric

import "sync"

// StatusWatchers is the set of things waiting to hear that a service's status changed.
//
// It belongs to the *service*, not to the Supervisor currently running it. That distinction is the
// whole reason this is its own type: `gozellij start` and `gozellij restart` replace the
// supervisor (see Fabric.replaceSupervisor), and a watcher bound to the old one is never told
// anything again. An attached client whose exit watcher had been quietly orphaned that way showed
// the service's output and then hung forever when it finished - the exact bug the watcher was
// added to fix, reintroduced one layer up by the object lifetime.
//
// Each watcher gets its own channel of capacity one. One shared channel is the obvious version and
// is wrong as soon as there are two watchers: a non-blocking send lands in whichever of them
// happens to be reading, so one client can swallow another's wakeup.
type StatusWatchers struct {
	mu    sync.Mutex
	chans map[int]chan struct{}
	next  int
}

// NewStatusWatchers makes an empty set.
func NewStatusWatchers() *StatusWatchers {
	return &StatusWatchers{chans: make(map[int]chan struct{})}
}

// Watch registers a watcher and returns its channel and a function to stop watching.
//
// The channel coalesces: one wakeup may cover several changes, so read the status after receiving
// rather than assuming one wakeup means one change. The stop function closes the channel, so a
// watcher ranging over it ends when it is cancelled, and it is safe to call more than once.
func (w *StatusWatchers) Watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	w.mu.Lock()
	id := w.next
	w.next++
	w.chans[id] = ch
	w.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			if c, ok := w.chans[id]; ok {
				delete(w.chans, id)
				close(c)
			}
			w.mu.Unlock()
		})
	}
}

// Notify wakes every watcher. The sends are non-blocking, so a watcher that is not reading cannot
// stall the supervision loop that is telling it.
func (w *StatusWatchers) Notify() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.chans {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Len reports how many watchers are registered. For tests.
func (w *StatusWatchers) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.chans)
}
