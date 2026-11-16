// Package access holds the rules for who may do what in a space: one level
// per visitor, the space's settings, and the decision for one request. It
// knows nothing about HTTP or storage.
//
// Keys are common (shared/) or owned by whoever wrote them (by/{id}/ and the
// to/ slots). Levels, lowest first:
//
//	banned  nothing, whatever else they hold
//	none    nothing: a stranger in a private space
//	read    read everything the space shows, and your own inbox
//	post    also write your own keys: by/me/ and to/
//	all     also write common keys: shared/
//	owner   also delete others' by/ keys, change settings, manage members
//
// A visitor's level is the highest of their live grants, or the space's
// anyone level if that is higher. A ban overrides both.
package access

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Level is how far a visitor may go in a space.
type Level int

// None is the zero value, so an unset level grants nothing.
const (
	Banned Level = iota - 1
	None
	Read
	Post
	All
	Owner
)

var levelNames = [...]string{"banned", "none", "read", "post", "all", "owner"}

func (l Level) String() string {
	if l < Banned || l > Owner {
		return "none"
	}
	return levelNames[l+1]
}

func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

func (l *Level) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, ok := ParseLevel(s)
	if !ok {
		return fmt.Errorf("unknown level %q", s)
	}
	*l = v
	return nil
}

// ParseLevel reads a level's name.
func ParseLevel(s string) (Level, bool) {
	for i, n := range levelNames {
		if n == s {
			return Level(i - 1), true
		}
	}
	return None, false
}

// LevelError is a level that isn't allowed where it was given.
type LevelError string

func (e LevelError) Error() string { return string(e) }

// ParseIn reads a JSON level and checks it is one of allowed.
func ParseIn(raw json.RawMessage, field string, allowed ...Level) (Level, error) {
	var s string
	json.Unmarshal(raw, &s)
	l, ok := ParseLevel(s)
	for _, a := range allowed {
		if ok && a == l {
			return l, nil
		}
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = fmt.Sprintf("%q", a.String())
	}
	return None, LevelError(fmt.Sprintf("%s is %s or %s.", field, strings.Join(names[:len(names)-1], ", "), names[len(names)-1]))
}

// Settings are a space's two access settings.
type Settings struct {
	// Anyone is the level of every visitor without a higher grant:
	// none (private, the default), read, post or all.
	Anyone Level `json:"anyone"`
	// Invite is the lowest member level that may mint invites: read,
	// post (the default), all or owner. The anyone level doesn't count.
	Invite Level `json:"invite"`
}

// Default is what a new space gets unless its creator says otherwise.
var Default = Settings{Anyone: None, Invite: Post}

// Encode is the compact form stored in a space file: "anyone invite".
func (s Settings) Encode() string { return s.Anyone.String() + " " + s.Invite.String() }

// Decode reads Encode's form. Anything unreadable is the default, which is
// private.
func Decode(v string) Settings {
	a, i, ok := strings.Cut(v, " ")
	anyone, ok1 := ParseLevel(a)
	invite, ok2 := ParseLevel(i)
	if !ok || !ok1 || !ok2 {
		return Default
	}
	return Settings{Anyone: anyone, Invite: invite}
}

// SettingsError is a problem with requested settings, for invalid_settings.
type SettingsError string

func (e SettingsError) Error() string { return string(e) }

// ApplyInput applies the anyone and invite fields of a request body to base.
// Fields left out keep base's value.
func ApplyInput(base Settings, anyone, invite json.RawMessage) (Settings, error) {
	s := base
	if anyone != nil {
		l, err := ParseIn(anyone, "anyone", None, Read, Post, All)
		if err != nil {
			return base, SettingsError(err.Error())
		}
		s.Anyone = l
	}
	if invite != nil {
		l, err := ParseIn(invite, "invite", Read, Post, All, Owner)
		if err != nil {
			return base, SettingsError(err.Error())
		}
		s.Invite = l
	}
	return s, nil
}

// Grant is one way a visitor got a level in a space.
type Grant struct {
	ID     string
	Level  Level // read, post, all or owner
	Source string
	Until  int64 // ms, 0 for no end
}

func (g Grant) Live(now time.Time) bool { return g.Until == 0 || g.Until > now.UnixMilli() }

// Rights is what one visitor holds in one space.
type Rights struct {
	Settings Settings
	// Member is the highest level among the visitor's live grants, None
	// without any.
	Member Level
	// Banned: an owner banned this visitor. Nothing else counts while it
	// lasts.
	Banned      bool
	BannedUntil int64 // ms, 0 for no end
}

// Level is what the visitor may do: their membership or the anyone level,
// whichever is higher, unless banned.
func (r *Rights) Level() Level {
	if r.Banned {
		return Banned
	}
	return max(r.Member, r.Settings.Anyone)
}

func (r *Rights) Owner() bool { return r.Level() == Owner }

// Visible reports whether the space shows itself to this visitor at all.
func (r *Rights) Visible() bool { return r.Level() >= Read }

// Op is what a request wants to do.
type Op struct {
	Area  string // "shared", "by" or "to"
	Write bool
}

// Need is the level an operation takes: reading anything takes read,
// writing your own keys post, writing common keys all. The fixed rules (by/
// keys only by their author, a to/ slot only by its sender, inbox/ always
// your own, writes need a visitor) are the caller's.
func (op Op) Need() Level {
	switch {
	case !op.Write:
		return Read
	case op.Area == "shared":
		return All
	}
	return Post
}

func (r *Rights) Allows(op Op) bool { return r.Level() >= op.Need() }

// CanInvite reports whether this visitor may mint invites: a member at the
// space's invite level or above. The anyone level doesn't count, so
// strangers in an open space can't fill it with tokens.
func (r *Rights) CanInvite() bool {
	return !r.Banned && r.Member >= Read && r.Member >= r.Settings.Invite
}

// CanGive reports whether this visitor may hand out level l: owners any,
// members up to their own.
func (r *Rights) CanGive(l Level) bool { return r.Member == Owner || l <= r.Member }

// ParseTime reads an until value: a duration from now ("7d", "12h"), a date
// (end of that day, server time zone), an RFC 3339 time with or without
// offset, or milliseconds since 1970. It returns ms.
func ParseTime(raw json.RawMessage, now time.Time) (int64, error) {
	var ms int64
	if json.Unmarshal(raw, &ms) == nil {
		if ms <= now.UnixMilli() {
			return 0, fmt.Errorf("that time is in the past")
		}
		return ms, nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, fmt.Errorf("a time is a duration like \"7d\", a date, an RFC 3339 time, or null")
	}
	if d, err := ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("the duration must be positive")
		}
		return now.Add(d).UnixMilli(), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		t = t.AddDate(0, 0, 1).Add(-time.Millisecond)
		if !t.After(now) {
			return 0, fmt.Errorf("that date is in the past")
		}
		return t.UnixMilli(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if !t.After(now) {
				return 0, fmt.Errorf("that time is in the past")
			}
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("%q isn't a duration, date or time", s)
}

// ParseDuration reads "30d", "12h", "90m" or "45s".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var v int
		if _, err := fmt.Sscanf(n, "%d", &v); err != nil || fmt.Sprint(v) != n {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(v) * 24 * time.Hour, nil
	}
	if !strings.ContainsAny(s, "hms") {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return time.ParseDuration(s)
}
