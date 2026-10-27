package server

import (
	"github.com/timjaanson/cubby/internal/config"
	"math"
	"sync"
	"time"
)

// limiter is a set of token buckets, one per key (an IP or a visitor ID).
type limiter struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const maxBuckets = 100_000

func newLimiter(perPeriod float64, period time.Duration, burst float64) *limiter {
	return &limiter{rate: perPeriod / period.Seconds(), burst: burst, now: time.Now, buckets: map[string]*bucket{}}
}

// take removes n tokens. If there aren't enough it returns how long until
// there will be, and takes nothing.
func (l *limiter) take(key string, n float64) (ok bool, retry time.Duration) {
	return l.use(key, n, true)
}

// peek reports whether n tokens are there without taking them.
func (l *limiter) peek(key string, n float64) (ok bool, retry time.Duration) {
	return l.use(key, n, false)
}

func (l *limiter) use(key string, n float64, take bool) (ok bool, retry time.Duration) {
	now := l.now()
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
		if take {
			b.tokens -= n
		}
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
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst && now.Sub(b.last) > time.Hour {
			delete(l.buckets, k)
		}
	}
}

type limits struct {
	visitorsPerIP    *limiter
	writesPerVisitor *limiter
	writesPerIP      *limiter
	writeBytesPerIP  *limiter
	readsPerIP       *limiter
	spacesPerVisitor *limiter
	spacesPerIP      *limiter
	pulsesPerVisitor *limiter
	pulsesPerIP      *limiter
	misses           *missGuard
}

func newLimits(configs ...*config.Config) *limits {
	cfg := config.Default()
	if len(configs) > 0 {
		cfg = configs[0]
	}
	c := cfg.Limits
	day := 24 * time.Hour
	return &limits{
		visitorsPerIP:    newLimiter(float64(c.VisitorsPerIPPerHour), time.Hour, float64(c.VisitorsPerIPPerHour)),
		writesPerVisitor: newLimiter(float64(c.WritesPerMinute), time.Minute, float64(c.WriteBurst)),
		writesPerIP:      newLimiter(float64(c.WritesPerIPPerMinute), time.Minute, float64(c.WritesPerIPPerMinute)),
		writeBytesPerIP:  newLimiter(float64(c.WriteBytesPerIPPerDay), day, float64(c.WriteBytesPerIPPerDay)),
		readsPerIP:       newLimiter(float64(c.ReadsPerIPPerMinute), time.Minute, float64(c.ReadsPerIPPerMinute)),
		spacesPerVisitor: newLimiter(float64(c.SpacesPerVisitorPerDay), day, float64(c.SpacesPerVisitorPerDay)),
		spacesPerIP:      newLimiter(float64(c.SpacesPerIPPerDay), day, float64(c.SpacesPerIPPerDay)),
		pulsesPerVisitor: newLimiter(float64(c.PulsesPerSecond), time.Second, float64(c.PulseBurst)),
		pulsesPerIP:      newLimiter(float64(c.PulsesPerIPPerSecond), time.Second, float64(2*c.PulsesPerIPPerSecond)),
		misses:           newMissGuard(c.MissesPerIPPerHour),
	}
}

func (l *limits) all() []*limiter {
	return []*limiter{l.visitorsPerIP, l.writesPerVisitor, l.writesPerIP, l.writeBytesPerIP, l.readsPerIP, l.spacesPerVisitor, l.spacesPerIP, l.pulsesPerVisitor, l.pulsesPerIP}
}

// watchCounter caps open watches per IP and in total.
type watchCounter struct {
	mu                     sync.Mutex
	perIP                  map[string]int
	total                  int
	perIPLimit, totalLimit int
}

const (
	maxWatchesPerIP = 50
	maxWatches      = 1000
)

func (w *watchCounter) acquire(ip string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	perIP, total := w.perIPLimit, w.totalLimit
	if perIP == 0 {
		perIP = maxWatchesPerIP
	}
	if total == 0 {
		total = maxWatches
	}
	if w.total >= total || w.perIP[ip] >= perIP {
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
