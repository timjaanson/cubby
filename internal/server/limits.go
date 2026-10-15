package server

import (
	"math"
	"sync"
	"time"
)

// limiter is a set of token buckets, one per key (an IP or a visitor ID).
type limiter struct {
	rate  float64 // tokens per second
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const maxBuckets = 100_000

func newLimiter(perPeriod float64, period time.Duration, burst float64) *limiter {
	return &limiter{rate: perPeriod / period.Seconds(), burst: burst, buckets: map[string]*bucket{}}
}

// take removes n tokens. If there aren't enough it returns how long until
// there will be, and takes nothing.
func (l *limiter) take(key string, n float64) (ok bool, retry time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			l.evictOldest()
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	need := n - b.tokens
	if n > l.burst {
		need = l.burst - b.tokens
	}
	return false, time.Duration(need / l.rate * float64(time.Second))
}

func (l *limiter) evictOldest() {
	var oldK string
	var oldT time.Time
	for k, b := range l.buckets {
		if oldK == "" || b.last.Before(oldT) {
			oldK, oldT = k, b.last
		}
	}
	delete(l.buckets, oldK)
}

// prune drops buckets that are full again, since a full bucket behaves like a new one.
func (l *limiter) prune() {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst && now.Sub(b.last) > time.Hour {
			delete(l.buckets, k)
		}
	}
}

type limits struct {
	anonPerIP        *limiter
	writesPerVisitor *limiter
	writesPerIP      *limiter
	writeBytesPerIP  *limiter
	readsPerIP       *limiter
	spacesPerVisitor *limiter
	spacesPerIP      *limiter
}

func newLimits() *limits {
	day := 24 * time.Hour
	return &limits{
		anonPerIP:        newLimiter(10, time.Hour, 10),
		writesPerVisitor: newLimiter(60, time.Minute, 30),
		writesPerIP:      newLimiter(300, time.Minute, 300),
		writeBytesPerIP:  newLimiter(10<<20, day, 10<<20),
		readsPerIP:       newLimiter(600, time.Minute, 600),
		spacesPerVisitor: newLimiter(5, day, 5),
		spacesPerIP:      newLimiter(20, day, 20),
	}
}

func (l *limits) all() []*limiter {
	return []*limiter{l.anonPerIP, l.writesPerVisitor, l.writesPerIP, l.writeBytesPerIP, l.readsPerIP, l.spacesPerVisitor, l.spacesPerIP}
}

// watchCounter caps open watches per IP and in total.
type watchCounter struct {
	mu    sync.Mutex
	perIP map[string]int
	total int
}

const (
	maxWatchesPerIP = 20
	maxWatches      = 1000
)

func (w *watchCounter) acquire(ip string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.total >= maxWatches || w.perIP[ip] >= maxWatchesPerIP {
		return false
	}
	w.total++
	w.perIP[ip]++
	return true
}

func (w *watchCounter) release(ip string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total--
	if w.perIP[ip]--; w.perIP[ip] <= 0 {
		delete(w.perIP, ip)
	}
}
