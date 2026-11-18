package access

import (
	"encoding/json"
	"testing"
	"time"
)

func raw(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func TestLevelNames(t *testing.T) {
	for _, n := range []string{"banned", "none", "read", "post", "all", "owner"} {
		l, ok := ParseLevel(n)
		if !ok || l.String() != n {
			t.Fatalf("%s: %v %v", n, l, ok)
		}
	}
	if _, ok := ParseLevel("member"); ok {
		t.Fatal("member is not a level")
	}
	var zero Level
	if zero != None {
		t.Fatal("the zero level must grant nothing")
	}
	if !(Banned < None && None < Read && Read < Post && Post < All && All < Owner) {
		t.Fatal("levels out of order")
	}
}

func TestSettings(t *testing.T) {
	s, err := ApplyInput(Default, raw(`"post"`), nil)
	if err != nil || s.Anyone != Post || s.Inviters != Post {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = ApplyInput(s, nil, raw(`"owner"`))
	if err != nil || s.Anyone != Post || s.Inviters != Owner {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range [][2]string{{`"owner"`, ""}, {`"banned"`, ""}, {`"respond"`, ""}, {"", `"none"`}, {"", `"banned"`}} {
		if _, err := ApplyInput(Default, raw(bad[0]), raw(bad[1])); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	if Decode(s.Encode()) != s {
		t.Fatalf("round trip %q", s.Encode())
	}
	if Decode("gggg") != Default || Decode("") != Default {
		t.Fatal("unreadable settings must be the private default")
	}
	b, _ := json.Marshal(s)
	if string(b) != `{"anyone":"post","inviters":"owner"}` {
		t.Fatal(string(b))
	}
	var back Settings
	if json.Unmarshal(b, &back) != nil || back != s {
		t.Fatalf("%+v", back)
	}
}

func TestRights(t *testing.T) {
	need := func(area string, write bool) Level { return Op{Area: area, Write: write}.Need() }
	if need("shared", false) != Read || need("by", true) != Post || need("to", true) != Post || need("shared", true) != All {
		t.Fatal("need")
	}

	// A stranger in a private space sees nothing.
	r := &Rights{Settings: Default}
	if r.Level() != None || r.Visible() || r.CanInvite() {
		t.Fatalf("stranger %+v", r)
	}
	// The anyone level counts for access, not for inviting.
	r.Settings.Anyone = All
	if r.Level() != All || !r.Allows(Op{Area: "shared", Write: true}) || r.CanInvite() {
		t.Fatalf("open space %+v", r)
	}
	// A member's own level counts when it's higher.
	r = &Rights{Settings: Settings{Anyone: Read, Inviters: Post}, Member: Post}
	if r.Level() != Post || !r.Allows(Op{Area: "by", Write: true}) || r.Allows(Op{Area: "shared", Write: true}) || !r.CanInvite() {
		t.Fatalf("post member %+v", r)
	}
	if !r.CanGive(Read) || !r.CanGive(Post) || r.CanGive(All) {
		t.Fatal("a member gives at most their own level")
	}
	r.Settings.Inviters = All
	if r.CanInvite() {
		t.Fatal("below the invite level")
	}
	// A ban beats everything.
	r = &Rights{Settings: Settings{Anyone: All, Inviters: Read}, Member: All, Banned: true}
	if r.Level() != Banned || r.Visible() || r.CanInvite() {
		t.Fatalf("banned %+v", r)
	}
	r = &Rights{Settings: Default, Member: Owner}
	if !r.Owner() || !r.CanGive(Owner) {
		t.Fatal("owner")
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"3600": time.Hour, "90s": 90 * time.Second, "10m": 10 * time.Minute, "12h": 12 * time.Hour, "14d": 14 * 24 * time.Hour} {
		if d, err := ParseDuration(in); err != nil || d != want {
			t.Errorf("%q: %v %v", in, d, err)
		}
	}
	for _, in := range []string{"", "0", "-5", "1h30m", "1.5h", "7 d", "soon", "d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
	if d, err := TTL(json.RawMessage(`60`)); err != nil || d != time.Minute {
		t.Fatal(d, err)
	}
	if d, err := TTL(json.RawMessage(`"2h"`)); err != nil || d != 2*time.Hour {
		t.Fatal(d, err)
	}
	if _, err := TTL(json.RawMessage(`"2026-01-01"`)); err == nil {
		t.Fatal("a date is not a duration")
	}
}
