package server

import (
	"encoding/json"
	"fmt"
	"github.com/timjaanson/cubby/internal/access"
	"strconv"
	"testing"
	"time"
)

// fakeGuard is a missGuard on a clock the test moves by hand, with a fixed
// set of spaces that exist.
type fakeGuard struct {
	*missGuard
	t      time.Time
	exists map[string]bool
}

func newFakeGuard() *fakeGuard {
	f := &fakeGuard{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), exists: map[string]bool{}}
	f.missGuard = newMissGuard()
	f.now = func() time.Time { return f.t }
	return f
}

func (f *fakeGuard) use(ip, space string) bool {
	ok, _ := f.check(ip, space, func() bool { return f.exists[space] })
	return ok
}

func (f *fakeGuard) wait(d time.Duration) { f.t = f.t.Add(d) }

func missing(i int) string { return fmt.Sprintf("guess-%04d-xxxxxxxxxx", i) }

func TestMissBudgetExact(t *testing.T) {
	g := newFakeGuard()
	for i := range missesPerHour {
		if !g.use("1.1.1.1", missing(i)) {
			t.Fatalf("miss %d of %d refused", i+1, missesPerHour)
		}
	}
	ok, retry := g.check("1.1.1.1", missing(999), func() bool { return false })
	if ok {
		t.Fatalf("miss %d allowed", missesPerHour+1)
	}
	// One token comes back every 2 minutes.
	if want := time.Hour / missesPerHour; retry < want-time.Second || retry > want+time.Second {
		t.Fatalf("retry = %v, want about %v", retry, want)
	}
}

func TestMissRepeatsAreFree(t *testing.T) {
	g := newFakeGuard()
	// A page polling a deleted space every 5 seconds for 3 hours.
	for range 3 * 720 {
		if !g.use("1.1.1.1", "deleted-space-xxxxxx") {
			t.Fatal("repeat of the same missing name refused")
		}
		g.wait(5 * time.Second)
	}
	// It cost one miss at most; the budget has long refilled anyway, but
	// check it's full right now without waiting.
	for i := range missesPerHour {
		if !g.use("1.1.1.1", missing(i)) {
			t.Fatalf("budget not full after polling: refused at %d", i+1)
		}
	}
}

func TestMissRepeatsDontRefillBudget(t *testing.T) {
	g := newFakeGuard()
	g.use("1.1.1.1", "deleted-space-xxxxxx")
	for range 1000 {
		g.use("1.1.1.1", "deleted-space-xxxxxx")
	}
	// 1 used, so exactly 29 more distinct misses fit.
	for i := range missesPerHour - 1 {
		if !g.use("1.1.1.1", missing(i)) {
			t.Fatalf("refused at %d, before the budget ran out", i+1)
		}
	}
	if g.use("1.1.1.1", missing(500)) {
		t.Fatal("allowed past the budget")
	}
}

func TestExistingSpacesCostNothing(t *testing.T) {
	g := newFakeGuard()
	for i := range 500 {
		name := fmt.Sprintf("real-%04d-xxxxxxxxxxx", i)
		g.exists[name] = true
		if !g.use("1.1.1.1", name) {
			t.Fatalf("existing space %d refused with a full budget", i)
		}
	}
	for i := range missesPerHour {
		if !g.use("1.1.1.1", missing(i)) {
			t.Fatalf("existing spaces used up budget: refused at %d", i+1)
		}
	}
}

func TestAlmostOutOfBudgetStillWorks(t *testing.T) {
	g := newFakeGuard()
	for i := range missesPerHour - 1 {
		g.use("1.1.1.1", missing(i))
	}
	g.exists["fresh-space-xxxxxxxx"] = true
	if !g.use("1.1.1.1", "fresh-space-xxxxxxxx") {
		t.Fatal("new existing space refused with 1 miss left")
	}
	if !g.use("1.1.1.1", missing(100)) {
		t.Fatal("last miss refused")
	}
}

func TestBlockedLooksTheSameForRealAndMissing(t *testing.T) {
	g := newFakeGuard()
	g.exists["known-space-xxxxxxxx"] = true
	g.exists["other-space-xxxxxxxx"] = true
	g.use("1.1.1.1", "known-space-xxxxxxxx")
	g.use("1.1.1.1", missing(0))
	for i := 1; i < missesPerHour; i++ {
		g.use("1.1.1.1", missing(i))
	}

	// Spaces used before the block keep working, existing or not.
	if !g.use("1.1.1.1", "known-space-xxxxxxxx") {
		t.Fatal("space in use was cut off")
	}
	if !g.use("1.1.1.1", missing(0)) {
		t.Fatal("missing space in use was cut off")
	}
	// New names are refused whether they exist or not.
	okReal, rReal := g.check("1.1.1.1", "other-space-xxxxxxxx", func() bool { return true })
	okMiss, rMiss := g.check("1.1.1.1", missing(777), func() bool { return false })
	if okReal || okMiss {
		t.Fatalf("blocked address got through: real %v, missing %v", okReal, okMiss)
	}
	if rReal != rMiss {
		t.Fatalf("retry differs: real %v, missing %v", rReal, rMiss)
	}
	// Refusals don't charge: after one refill period exactly one new name fits.
	g.wait(time.Hour / missesPerHour)
	if !g.use("1.1.1.1", "other-space-xxxxxxxx") {
		t.Fatal("refused after one token came back")
	}
	if !g.use("1.1.1.1", missing(778)) {
		t.Fatal("an existing space spent the refilled token")
	}
	if g.use("1.1.1.1", missing(779)) {
		t.Fatal("two misses fit in one refill period")
	}
}

func TestMissRefill(t *testing.T) {
	g := newFakeGuard()
	for i := range missesPerHour {
		g.use("1.1.1.1", missing(i))
	}
	g.wait(time.Hour/missesPerHour - time.Second)
	if g.use("1.1.1.1", missing(100)) {
		t.Fatal("token back too early")
	}
	g.wait(time.Second)
	if !g.use("1.1.1.1", missing(101)) {
		t.Fatal("token not back after 2 minutes")
	}
	if g.use("1.1.1.1", missing(102)) {
		t.Fatal("more than one token back")
	}
	g.wait(time.Hour)
	n := 0
	for i := range 100 {
		if g.use("1.1.1.1", missing(200+i)) {
			n++
		}
	}
	if n != missesPerHour {
		t.Fatalf("after an hour %d misses fit, want %d (burst is capped)", n, missesPerHour)
	}
}

func TestSlowGuessingStaysSlow(t *testing.T) {
	g := newFakeGuard()
	// Probing as fast as allowed for a day: 30 up front, then one per 2 minutes.
	n := 0
	for i := range 24 * 3600 {
		if g.use("1.1.1.1", "probe-"+strconv.Itoa(i)+"-xxxxxxxxxx") {
			n++
		}
		g.wait(time.Second)
	}
	if want := missesPerHour + 24*missesPerHour; n < want-2 || n > want+2 {
		t.Fatalf("%d guesses in a day, want about %d", n, want)
	}
}

func TestSeenExpires(t *testing.T) {
	g := newFakeGuard()
	g.use("1.1.1.1", "deleted-space-xxxxxx")
	for i := 1; i < missesPerHour; i++ {
		g.use("1.1.1.1", missing(i))
	}
	// Still used within the hour: free.
	g.wait(59 * time.Minute)
	if !g.use("1.1.1.1", "deleted-space-xxxxxx") {
		t.Fatal("name used 59 minutes ago was refused")
	}
	// Each use renews it, so 59 minutes later it's still free.
	g.wait(59 * time.Minute)
	g.missGuard.budget.buckets["1.1.1.1"].tokens = 0 // keep the address blocked
	g.missGuard.budget.buckets["1.1.1.1"].last = g.t
	if !g.use("1.1.1.1", "deleted-space-xxxxxx") {
		t.Fatal("renewed name was refused")
	}
	// Left alone for an hour it's forgotten and counts as new.
	g.wait(time.Hour)
	g.missGuard.budget.buckets["1.1.1.1"].tokens = 0
	g.missGuard.budget.buckets["1.1.1.1"].last = g.t
	if g.use("1.1.1.1", "deleted-space-xxxxxx") {
		t.Fatal("name unused for an hour was still free")
	}
}

func TestMissAddressesAreSeparate(t *testing.T) {
	g := newFakeGuard()
	for i := range missesPerHour {
		g.use("1.1.1.1", missing(i))
	}
	if g.use("1.1.1.1", missing(100)) {
		t.Fatal("first address not blocked")
	}
	for i := range missesPerHour {
		if !g.use("2.2.2.2", missing(i)) {
			t.Fatalf("second address refused at %d", i+1)
		}
	}
}

func TestSeenCaps(t *testing.T) {
	g := newFakeGuard()
	for i := range maxSeenPerIP + 50 {
		name := fmt.Sprintf("real-%04d-xxxxxxxxxxx", i)
		g.exists[name] = true
		g.use("1.1.1.1", name)
		g.wait(time.Millisecond)
	}
	if n := len(g.seen["1.1.1.1"].names); n != maxSeenPerIP {
		t.Fatalf("%d names remembered, want %d", n, maxSeenPerIP)
	}
	if _, ok := g.seen["1.1.1.1"].names["real-0000-xxxxxxxxxxx"]; ok {
		t.Fatal("oldest name not evicted")
	}
}

func TestMissPrune(t *testing.T) {
	g := newFakeGuard()
	g.use("1.1.1.1", missing(1))
	g.wait(30 * time.Minute)
	g.use("2.2.2.2", missing(2))
	g.wait(31 * time.Minute)
	g.prune()
	if _, ok := g.seen["1.1.1.1"]; ok {
		t.Fatal("expired address kept")
	}
	if _, ok := g.seen["2.2.2.2"]; !ok {
		t.Fatal("live address dropped")
	}
}

// TestMissLimitHTTP runs the guard through the real server and routing.
func TestMissLimitHTTP(t *testing.T) {
	c := newTest(t)
	if !c.cfg.RateLimitsOn() {
		t.Fatal("test needs rate limits on")
	}
	as := func(ip string) []string { return []string{"X-Forwarded-For", ip} }
	ip := as("203.0.113.7")
	ownerTok, owner := c.anon()
	for _, n := range []string{"inuse-space-xxxxxxxx", "never-touched-xxxxxx"} {
		if _, _, err := c.store.CreateSpace(n, "", access.Settings{Read: true}, owner); err != nil {
			t.Fatal(err)
		}
	}
	// A private space looks exactly like a missing one, and costs a miss.
	if _, _, err := c.store.CreateSpace("private-xxxxxxxxxxx", "", access.Private, owner); err != nil {
		t.Fatal(err)
	}

	// Missing keys in a real space never count.
	for range 100 {
		if s, _, b := c.req("GET", "/v1/inuse-space-xxxxxxxx/shared/nope", "", ip...); s != 404 || errCode(b) != "not_found" {
			t.Fatalf("missing key: %d %s", s, b)
		}
	}
	// Exactly the budget of distinct missing spaces, each a plain 404. One
	// of them is the private space, which must give the same answer.
	_, hm, missBody := c.req("GET", "/v1/"+missing(1)+"/shared/k", "", ip...)
	_, hp, privBody := c.req("GET", "/v1/private-xxxxxxxxxxx/shared/k", "", ip...)
	if errCode(missBody) != "no_space" || missBody != privBody || len(hm) != len(hp) {
		t.Fatalf("private differs from missing: %s / %s", missBody, privBody)
	}
	for i := range missesPerHour - 1 {
		if s, _, b := c.req("GET", "/v1/"+missing(i)+"/shared/k", "", ip...); s != 404 || errCode(b) != "no_space" {
			t.Fatalf("miss %d: %d %s", i+1, s, b)
		}
	}
	// The private space's owner, from the same address, isn't affected:
	// they used it.
	_ = ownerTok
	// Repeats of those are still plain 404s.
	if s, _, _ := c.req("GET", "/v1/"+missing(0)+"/shared/k", "", ip...); s != 404 {
		t.Fatalf("repeat miss: %d", s)
	}
	// One more distinct name is refused, with a Retry-After of about 2 minutes.
	s, h, b := c.req("GET", "/v1/"+missing(500)+"/shared/k", "", ip...)
	if s != 429 || errCode(b) != "miss_limit" {
		t.Fatalf("over budget: %d %s", s, b)
	}
	if ra, _ := strconv.Atoi(h.Get("Retry-After")); ra < 110 || ra > 121 {
		t.Fatalf("Retry-After = %q", h.Get("Retry-After"))
	}
	// A real space this address never used gets the same answer.
	s2, _, b2 := c.req("GET", "/v1/never-touched-xxxxxx/shared/k", "", ip...)
	if s2 != 429 || errCode(b2) != "miss_limit" {
		t.Fatalf("unused real space while blocked: %d %s", s2, b2)
	}
	// The space it was using keeps working, reads and writes.
	if s, _, _ := c.req("GET", "/v1/inuse-space-xxxxxxxx/shared/nope", "", ip...); s != 404 {
		t.Fatalf("space in use cut off: %d", s)
	}
	if s, _, _ := c.req("GET", "/v1/inuse-space-xxxxxxxx/shared/*", "", ip...); s != 200 {
		t.Fatalf("list in use cut off: %d", s)
	}
	// Another address, and another IPv6 /64, are unaffected; the same /64 isn't.
	if s, _, _ := c.req("GET", "/v1/"+missing(500)+"/shared/k", "", as("203.0.113.8")...); s != 404 {
		t.Fatalf("other address: %d", s)
	}
	v6 := as("2001:db8:1:2::1")
	for i := range missesPerHour {
		c.req("GET", "/v1/"+missing(i)+"/shared/k", "", v6...)
	}
	if s, _, _ := c.req("GET", "/v1/"+missing(600)+"/shared/k", "", as("2001:db8:1:2:ffff::9")...); s != 429 {
		t.Fatalf("same /64: %d", s)
	}
	if s, _, _ := c.req("GET", "/v1/"+missing(600)+"/shared/k", "", as("2001:db8:1:3::1")...); s != 404 {
		t.Fatalf("other /64: %d", s)
	}
	// A space created while blocked is usable by its creator right away.
	tokB, _ := c.anon()
	_, _, b = c.req("POST", "/v1/spaces", `{"label":"late"}`, "Authorization", "Bearer "+tokB, "X-Forwarded-For", "203.0.113.7")
	var created struct{ Space string }
	json.Unmarshal([]byte(b), &created)
	if created.Space == "" {
		t.Fatalf("create while blocked: %s", b)
	}
	if s, _, b := c.req("GET", "/v1/"+created.Space+"/shared/k", "", append(ip, "Authorization", "Bearer "+tokB)...); s != 404 || errCode(b) != "not_found" {
		t.Fatalf("new space refused to its creator: %d %s", s, b)
	}
	// Creating spaces with POST /v1/spaces isn't a miss, and using them after is free.
	tok, _ := c.anon()
	auth := []string{"Authorization", "Bearer " + tok, "X-Forwarded-For", "198.51.100.1"}
	for i := range 3 {
		if s, _, b := c.req("POST", "/v1/spaces", `{"label":"room"}`, auth...); s != 201 {
			t.Fatalf("create %d: %d %s", i, s, b)
		}
	}
	for i := range missesPerHour {
		if s, _, _ := c.req("GET", "/v1/"+missing(i)+"/shared/k", "", "X-Forwarded-For", "198.51.100.1"); s != 404 {
			t.Fatalf("creating spaces used miss budget: refused at %d", i+1)
		}
	}
}

func TestMissLimitOff(t *testing.T) {
	c := newTest(t)
	c.cfg.Server.RateLimits = new(bool)
	for i := range 3 * missesPerHour {
		if s, _, _ := c.req("GET", "/v1/"+missing(i)+"/shared/k", ""); s != 404 {
			t.Fatalf("limits off, miss %d: %d", i+1, s)
		}
	}
}
