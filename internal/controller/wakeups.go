package controller

import "sync"

// wakeups lets callers wait for something to happen to a key: a wait's
// channel is closed by the next wake of its key. Take the channel before
// looking at what it guards, so a wake in between is not missed. The zero
// value is ready to use.
type wakeups struct {
	mu    sync.Mutex
	chans map[string]chan struct{}
}

// wait is a channel closed when key is next woken.
func (w *wakeups) wait(key string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.chans == nil {
		w.chans = map[string]chan struct{}{}
	}
	ch, ok := w.chans[key]
	if !ok {
		ch = make(chan struct{})
		w.chans[key] = ch
	}
	return ch
}

// wake wakes everyone waiting on key.
func (w *wakeups) wake(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch, ok := w.chans[key]; ok {
		close(ch)
		delete(w.chans, key)
	}
}
