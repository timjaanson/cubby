package server

import (
	"sync"
	"time"
)

// missGuard makes guessing space names slow. Each address may touch a budget
// of distinct spaces that don't exist (30 an hour). Touching the same missing
// name again is free, so a page polling a deleted space costs one miss, not
// one per poll. Missing keys inside a space that exists cost nothing.
//
// Once an address is out of budget, every space it hasn't used in the last
// hour answers 429, whether the space exists or not. Spaces it was already
// using keep working. Answering 429 only for missing spaces would tell a
// guesser exactly which names exist.
type missGuard struct {
	budget *limiter
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]*seenSet // address -> spaces it used recently
}

type seenSet struct {
	names map[string]time.Time
	last  time.Time
}

const (
	missesPerHour = 30
	seenTTL       = time.Hour
	maxSeenPerIP  = 256
	maxSeenIPs    = 100_000
)

func newMissGuard(limits ...int) *missGuard {
	budget := missesPerHour
	if len(limits) > 0 {
		budget = limits[0]
	}
	g := &missGuard{budget: newLimiter(float64(budget), time.Hour, float64(budget)), now: time.Now, seen: map[string]*seenSet{}}
	g.budget.now = g.clock
	return g
}

func (g *missGuard) clock() time.Time { return g.now() }

// check decides whether ip may use space. exists is only called for spaces
// the address hasn't used recently.
func (g *missGuard) check(ip, space string, exists func() bool) (ok bool, retry time.Duration) {
	now := g.now()
	g.mu.Lock()
	if set := g.seen[ip]; set != nil {
		if t, hit := set.names[space]; hit && now.Sub(t) < seenTTL {
			set.names[space], set.last = now, now
			g.mu.Unlock()
			return true, 0
		}
	}
	g.mu.Unlock()

	if exists() {
		ok, retry = g.budget.peek(ip, 1)
	} else {
		ok, retry = g.budget.take(ip, 1)
	}
	if ok {
		g.remember(ip, space, now)
	}
	return ok, retry
}

// allowed reports whether ip may try space at all without spending a miss:
// it used the space recently, or it has budget left. join uses it, then
// charges a miss only if the attempt fails.
func (g *missGuard) allowed(ip, space string) (ok bool, retry time.Duration) {
	now := g.now()
	g.mu.Lock()
	if set := g.seen[ip]; set != nil {
		if t, hit := set.names[space]; hit && now.Sub(t) < seenTTL {
			g.mu.Unlock()
			return true, 0
		}
	}
	g.mu.Unlock()
	return g.budget.peek(ip, 1)
}

// charge spends one miss.
func (g *missGuard) charge(ip string) { g.budget.take(ip, 1) }

func (g *missGuard) remember(ip, space string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.seen[ip]
	if set == nil {
		if len(g.seen) >= maxSeenIPs {
			g.evictOldestIP()
		}
		set = &seenSet{names: map[string]time.Time{}}
		g.seen[ip] = set
	}
	if _, hit := set.names[space]; !hit && len(set.names) >= maxSeenPerIP {
		var oldK string
		var oldT time.Time
		for k, t := range set.names {
			if oldK == "" || t.Before(oldT) {
				oldK, oldT = k, t
			}
		}
		delete(set.names, oldK)
	}
	set.names[space], set.last = now, now
}

func (g *missGuard) evictOldestIP() {
	var oldK string
	var oldT time.Time
	for k, s := range g.seen {
		if oldK == "" || s.last.Before(oldT) {
			oldK, oldT = k, s.last
		}
	}
	delete(g.seen, oldK)
}

// prune drops names not used within seenTTL, and addresses left with none.
func (g *missGuard) prune() {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for ip, set := range g.seen {
		for k, t := range set.names {
			if now.Sub(t) >= seenTTL {
				delete(set.names, k)
			}
		}
		if len(set.names) == 0 {
			delete(g.seen, ip)
		}
	}
	g.budget.prune()
}
