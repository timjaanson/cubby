package server

import (
	"sync"
	"time"
)

type notifier struct {
	mu     sync.Mutex
	spaces map[string]*spaceSignal
}

type spaceSignal struct {
	ch      chan struct{}
	waiters int
	last    time.Time
	pause   bool
}

// subscribe returns a channel that closes on the next write to the space.
func (n *notifier) subscribe(space string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.spaces[space]
	if s == nil {
		s = &spaceSignal{ch: make(chan struct{})}
		n.spaces[space] = s
	}
	return s.ch
}

func (n *notifier) join(space string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.spaces[space]
	if s == nil {
		s = &spaceSignal{ch: make(chan struct{})}
		n.spaces[space] = s
	}
	s.waiters++
}

func (n *notifier) leave(space string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := n.spaces[space]; s != nil {
		if s.waiters--; s.waiters <= 0 {
			delete(n.spaces, space)
		}
	}
}

func (n *notifier) fire(space string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := n.spaces[space]; s != nil {
		now := time.Now()
		s.pause = !s.last.IsZero() && now.Sub(s.last) < 50*time.Millisecond
		s.last = now
		close(s.ch)
		s.ch = make(chan struct{})
	}
}

func (n *notifier) burst(space string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.spaces[space]
	return s != nil && s.pause && time.Since(s.last) < 50*time.Millisecond
}
