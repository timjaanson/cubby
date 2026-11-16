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
	if err != nil || s.Anyone != Post || s.Invite != Post {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = ApplyInput(s, nil, raw(`"owner"`))
	if err != nil || s.Anyone != Post || s.Invite != Owner {
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
	if string(b) != `{"anyone":"post","invite":"owner"}` {
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
	r = &Rights{Settings: Settings{Anyone: Read, Invite: Post}, Member: Post}
	if r.Level() != Post || !r.Allows(Op{Area: "by", Write: true}) || r.Allows(Op{Area: "shared", Write: true}) || !r.CanInvite() {
		t.Fatalf("post member %+v", r)
	}
	if !r.CanGive(Read) || !r.CanGive(Post) || r.CanGive(All) {
		t.Fatal("a member gives at most their own level")
	}
	r.Settings.Invite = All
	if r.CanInvite() {
		t.Fatal("below the invite level")
	}
	// A ban beats everything.
	r = &Rights{Settings: Settings{Anyone: All, Invite: Read}, Member: All, Banned: true}
	if r.Level() != Banned || r.Visible() || r.CanInvite() {
		t.Fatalf("banned %+v", r)
	}
	r = &Rights{Settings: Default, Member: Owner}
	if !r.Owner() || !r.CanGive(Owner) {
		t.Fatal("owner")
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	if ms, err := ParseTime(raw(`"7d"`), now); err != nil || ms != now.Add(7*24*time.Hour).UnixMilli() {
		t.Fatal("duration", err)
	}
	if ms, err := ParseTime(raw(`"2026-10-23"`), now); err != nil || time.UnixMilli(ms).Day() != 23 || time.UnixMilli(ms).Hour() != 23 {
		t.Fatal("date", err)
	}
	if _, err := ParseTime(raw(`"2026-10-23T18:00"`), now); err != nil {
		t.Fatal("local time", err)
	}
	if _, err := ParseTime(raw(`"2020-01-01"`), now); err == nil {
		t.Fatal("past accepted")
	}
	if _, err := ParseTime(raw(`"soon"`), now); err == nil {
		t.Fatal("garbage accepted")
	}
}
