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

func TestPublicShorthand(t *testing.T) {
	for _, p := range []string{"none", "read", "respond", "all"} {
		s, err := ApplyInput(Private, raw(`"`+p+`"`), nil, nil, nil)
		if err != nil || s.Public() != p {
			t.Fatalf("%s: %v %v", p, s.Public(), err)
		}
		if Decode(s.Encode()) != s {
			t.Fatalf("%s: encode round trip", p)
		}
	}
	if Private.Public() != "none" {
		t.Fatal("default isn't private")
	}
	s, err := ApplyInput(presets["all"], nil, nil, raw(`{"me": "granted"}`), nil)
	if err != nil || s.Public() != "custom" || !s.WriteShared || s.WriteMe {
		t.Fatalf("custom: %+v %v", s, err)
	}
	if _, err := ApplyInput(Private, raw(`"read"`), raw(`"anyone"`), nil, nil); err == nil {
		t.Fatal("public with read accepted")
	}
	if _, err := ApplyInput(Private, nil, nil, raw(`{"shared": "anyone"}`), nil); err == nil {
		t.Fatal("open write into a private space accepted")
	}
	if _, err := ApplyInput(Private, raw(`"open"`), nil, nil, nil); err == nil {
		t.Fatal("unknown preset accepted")
	}
	b, _ := json.Marshal(presets["respond"])
	if string(b) != `{"public":"respond","read":"anyone","write":{"me":"anyone","shared":"granted","users":"anyone"},"invite":"members"}` {
		t.Fatalf("json: %s", b)
	}
}

func TestScopes(t *testing.T) {
	cases := []struct {
		scope string
		op    Op
		want  bool
	}{
		{"*:rw", Op{Area: "me", Key: "x", Write: true}, true},
		{"shared:r", Op{Area: "shared", Key: "x"}, true},
		{"shared:r", Op{Area: "shared", Key: "x", Write: true}, false},
		{"shared/env.*:w", Op{Area: "shared", Key: "env.temp", Write: true}, true},
		{"shared/env.*:w", Op{Area: "shared", Key: "env.", Pattern: true, Write: true}, true},
		{"shared/env.*:w", Op{Area: "shared", Key: "env.t", Pattern: true, Write: true}, true},
		{"shared/env.*:w", Op{Area: "shared", Key: "e", Pattern: true, Write: true}, false},
		{"shared/env.*:w", Op{Area: "shared", Key: "", Pattern: true, Write: true}, false},
		{"shared/config:w", Op{Area: "shared", Key: "config", Write: true}, true},
		{"shared/config:w", Op{Area: "shared", Key: "config2", Write: true}, false},
		{"shared/config:w", Op{Area: "shared", Key: "config", Pattern: true, Write: true}, false},
		{"users:w", Op{Area: "users", Key: "vote", Write: true}, true},
		{"users:w", Op{Area: "shared", Key: "vote", Write: true}, false},
	}
	for _, c := range cases {
		sc, err := ParseScope(c.scope)
		if err != nil {
			t.Fatal(err)
		}
		if sc.Covers(c.op) != c.want {
			t.Errorf("%s covers %+v: want %v", c.scope, c.op, c.want)
		}
	}
	for _, bad := range []string{"shared", "shared:x", "sharde:r", "shared/a/b:r", "shared/:r", "shared/watch:r", ":r"} {
		if _, err := ParseScope(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if a, _ := ParseAllow(nil); len(a) != 1 || a[0] != "*:rw" {
		t.Fatalf("default allow %v", a)
	}
	if a, _ := ParseAllow(raw(`"viewer"`)); len(a) != 2 {
		t.Fatalf("viewer %v", a)
	}
	if a, err := ParseAllow(raw(`["shared/*:rw", "me:rw"]`)); err != nil || a[0] != "shared:rw" {
		t.Fatalf("list %v %v", a, err)
	}
	if _, err := ParseAllow(raw(`"editor"`)); err == nil {
		t.Fatal("unknown preset accepted")
	}
}

func TestRights(t *testing.T) {
	r := Rights{Settings: presets["respond"]}
	if !r.Allows(Op{Area: "users", Key: "vote", Write: true}) || r.Allows(Op{Area: "shared", Key: "q", Write: true}) {
		t.Fatal("respond")
	}
	r.Grants = []Grant{{Scopes: ParseScopes([]string{"shared/q:w"})}}
	if !r.Allows(Op{Area: "shared", Key: "q", Write: true}) {
		t.Fatal("grant")
	}
	p := Rights{Settings: Private}
	if p.Visible() || p.Allows(Op{Area: "shared", Key: "x"}) {
		t.Fatal("private")
	}
	p.Grants = []Grant{{Scopes: ParseScopes([]string{"shared/env.*:w"})}}
	if !p.Visible() || p.Allows(Op{Area: "shared", Key: "env.t"}) {
		t.Fatal("write-only grant")
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

func TestSettingsJSONRoundTrip(t *testing.T) {
	for _, s := range []Settings{{}, {Read: true}, {Read: true, WriteUsers: true, WriteMe: true}, {Read: true, WriteShared: true}} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var got Settings
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if got != s {
			t.Fatalf("%s: got %+v, want %+v", b, got, s)
		}
	}
}

func TestInviteSetting(t *testing.T) {
	if Private.Invite() != "members" {
		t.Fatal("members invite by default")
	}
	s, err := ApplyInput(Private, nil, nil, nil, raw(`"owners"`))
	if err != nil || !s.InviteOwners || s.Public() != "none" {
		t.Fatalf("owners: %+v %v", s, err)
	}
	if Decode(s.Encode()) != s || Decode("gggg") != Private {
		t.Fatal("encode round trip, old four-letter form is members")
	}
	s2, err := ApplyInput(s, raw(`"read"`), nil, nil, nil)
	if err != nil || !s2.InviteOwners || s2.Public() != "read" {
		t.Fatalf("public keeps invite: %+v %v", s2, err)
	}
	if _, err := ApplyInput(Private, nil, nil, nil, raw(`"everyone"`)); err == nil {
		t.Fatal("bad invite accepted")
	}
	var back Settings
	b, _ := json.Marshal(s)
	if json.Unmarshal(b, &back) != nil || back != s {
		t.Fatalf("json round trip %s", b)
	}
}

func TestCanGive(t *testing.T) {
	member := func(allow ...string) *Rights {
		return &Rights{Grants: []Grant{{Allow: allow, Scopes: ParseScopes(allow)}}}
	}
	cases := []struct {
		have, give []string
		ok         bool
	}{
		{[]string{"*:rw"}, []string{"*:rw"}, true},
		{[]string{"*:rw"}, []string{"shared/x:w", "users:r"}, true},
		{[]string{"shared:r", "users:r"}, []string{"*:rw"}, false},
		{[]string{"shared:r", "users:r"}, []string{"shared:r"}, true},
		{[]string{"shared:r"}, []string{"shared:rw"}, false},
		{[]string{"shared/board.*:rw"}, []string{"shared/board.cards*:w"}, true},
		{[]string{"shared/board.*:rw"}, []string{"shared:r"}, false},
		{[]string{"shared/note:rw"}, []string{"shared/note:r"}, true},
		{[]string{"shared/note:rw"}, []string{"shared/note.*:r"}, false},
		{[]string{"shared:r", "shared:w"}, []string{"shared:rw"}, true},
	}
	for _, c := range cases {
		if got := member(c.have...).CanGive(c.give); got != c.ok {
			t.Errorf("have %v give %v: %v", c.have, c.give, got)
		}
	}
	r := member("shared:r")
	if !r.CanInvite() {
		t.Fatal("member invites by default")
	}
	r.Settings.InviteOwners = true
	if r.CanInvite() || (&Rights{}).CanInvite() || !(&Rights{Owner: true, Settings: Settings{InviteOwners: true}}).CanInvite() {
		t.Fatal("invite owners")
	}
	if got := (&Rights{Grants: []Grant{{Allow: []string{"shared:r"}}, {Allow: []string{"shared:r", "me:rw"}}}}).OwnAllow(); len(got) != 2 {
		t.Fatalf("own allow %v", got)
	}
}
