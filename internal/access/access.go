// Package access holds the rules for who may do what in a space: settings
// and their public shorthand, scopes and their presets, and the decision for
// one request. It knows nothing about HTTP or storage.
package access

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/timjaanson/cubby/internal/names"
)

// Settings say what a visitor may do without a grant. true means anyone.
type Settings struct {
	Read        bool
	WriteShared bool
	WriteUsers  bool
	WriteMe     bool
}

// The public shorthand. Private is the default.
var presets = map[string]Settings{
	"none":    {},
	"read":    {Read: true},
	"respond": {Read: true, WriteUsers: true, WriteMe: true},
	"all":     {Read: true, WriteShared: true, WriteUsers: true, WriteMe: true},
}

// Private is the default for new spaces.
var Private = Settings{}

// Public names the settings, or "custom".
func (s Settings) Public() string {
	for _, n := range []string{"none", "read", "respond", "all"} {
		if presets[n] == s {
			return n
		}
	}
	return "custom"
}

// Valid: an unreadable space can't take writes from anyone, since an open
// write would tell a stranger the space exists.
func (s Settings) Valid() bool {
	return s.Read || !(s.WriteShared || s.WriteUsers || s.WriteMe)
}

// WriteAnyone reports the write setting of one area: "shared", "users" or "me".
func (s Settings) WriteAnyone(area string) bool {
	switch area {
	case "shared":
		return s.WriteShared
	case "users":
		return s.WriteUsers
	case "me":
		return s.WriteMe
	}
	return false
}

func word(b bool) string {
	if b {
		return "anyone"
	}
	return "granted"
}

type settingsJSON struct {
	Public string            `json:"public"`
	Read   string            `json:"read"`
	Write  map[string]string `json:"write"`
}

func (s Settings) MarshalJSON() ([]byte, error) {
	return json.Marshal(settingsJSON{Public: s.Public(), Read: word(s.Read),
		Write: map[string]string{"shared": word(s.WriteShared), "users": word(s.WriteUsers), "me": word(s.WriteMe)}})
}

// Encode is the compact form stored in a space file.
func (s Settings) Encode() string {
	b := func(v bool) byte {
		if v {
			return 'a'
		}
		return 'g'
	}
	return string([]byte{b(s.Read), b(s.WriteShared), b(s.WriteUsers), b(s.WriteMe)})
}

// Decode reads Encode's form. Anything unreadable is private.
func Decode(v string) Settings {
	if len(v) != 4 {
		return Private
	}
	return Settings{Read: v[0] == 'a', WriteShared: v[1] == 'a', WriteUsers: v[2] == 'a', WriteMe: v[3] == 'a'}
}

// SettingsError is a problem with requested settings, for invalid_settings.
type SettingsError string

func (e SettingsError) Error() string { return string(e) }

// ApplyInput applies the settings fields of a request body to base: either
// public, or read and write. Fields left out keep base's value. raw holds
// only the fields that were present.
func ApplyInput(base Settings, public, read, write json.RawMessage) (Settings, error) {
	if public != nil {
		if read != nil || write != nil {
			return base, SettingsError("Give either public, or read and write, not both.")
		}
		var p string
		if json.Unmarshal(public, &p) != nil {
			return base, SettingsError(`public is "none", "read", "respond" or "all".`)
		}
		s, ok := presets[p]
		if !ok {
			return base, SettingsError(`public is "none", "read", "respond" or "all".`)
		}
		return s, nil
	}
	s := base
	if read != nil {
		var r string
		json.Unmarshal(read, &r)
		switch r {
		case "anyone":
			s.Read = true
		case "granted":
			s.Read = false
		default:
			return base, SettingsError(`read is "anyone" or "granted".`)
		}
	}
	if write != nil {
		var all string
		if json.Unmarshal(write, &all) == nil {
			switch all {
			case "anyone":
				s.WriteShared, s.WriteUsers, s.WriteMe = true, true, true
			case "granted":
				s.WriteShared, s.WriteUsers, s.WriteMe = false, false, false
			default:
				return base, SettingsError(`write is "anyone", "granted", or an object like {"shared": "granted"}.`)
			}
		} else {
			var m map[string]string
			if json.Unmarshal(write, &m) != nil {
				return base, SettingsError(`write is "anyone", "granted", or an object like {"shared": "granted"}.`)
			}
			for area, v := range m {
				if v != "anyone" && v != "granted" {
					return base, SettingsError(fmt.Sprintf(`write.%s is "anyone" or "granted".`, area))
				}
				switch area {
				case "shared":
					s.WriteShared = v == "anyone"
				case "users":
					s.WriteUsers = v == "anyone"
				case "me":
					s.WriteMe = v == "anyone"
				default:
					return base, SettingsError(fmt.Sprintf("write has no area %q. Areas are shared, users and me.", area))
				}
			}
		}
	}
	if !s.Valid() {
		return base, SettingsError(`A space with read "granted" can't let anyone write. Use public "read", "respond" or "all" to open it up.`)
	}
	return s, nil
}

// Scope is one entry of a grant's allow list: AREA[/TARGET]:RIGHTS.
type Scope struct {
	Area    string // "shared", "users", "me" or "*"
	Prefix  string // the target: a key, or a prefix when Pattern
	Pattern bool
	R, W    bool
}

const MaxScopes = 20

// Presets for allow lists.
var allowPresets = map[string][]string{
	"member": {"*:rw"},
	"viewer": {"shared:r", "users:r"},
}

// DefaultAllow is what a token or direct grant gets without allow.
var DefaultAllow = allowPresets["member"]

func ParseScope(s string) (Scope, error) {
	var sc Scope
	body, rights, ok := strings.Cut(s, ":")
	if !ok || strings.Contains(rights, ":") {
		return sc, fmt.Errorf("%q: a scope is AREA[/TARGET]:RIGHTS, like shared/status.*:w", s)
	}
	switch rights {
	case "r":
		sc.R = true
	case "w":
		sc.W = true
	case "rw":
		sc.R, sc.W = true, true
	default:
		return sc, fmt.Errorf("%q: rights are r, w or rw", s)
	}
	area, target, hasTarget := strings.Cut(body, "/")
	switch area {
	case "shared", "users", "me", "*":
		sc.Area = area
	default:
		return sc, fmt.Errorf("%q: the area is shared, users, me or *", s)
	}
	if !hasTarget || target == "*" {
		sc.Pattern = true
		return sc, nil
	}
	switch {
	case names.ValidPattern(target):
		sc.Pattern, sc.Prefix = true, strings.TrimSuffix(target, "*")
	case names.ValidKey(target):
		sc.Prefix = target
	default:
		return sc, fmt.Errorf("%q: the target is a key or a pattern like status.*", s)
	}
	return sc, nil
}

func (sc Scope) String() string {
	t := ""
	switch {
	case sc.Pattern && sc.Prefix == "":
	case sc.Pattern:
		t = "/" + sc.Prefix + "*"
	default:
		t = "/" + sc.Prefix
	}
	r := "r"
	switch {
	case sc.R && sc.W:
		r = "rw"
	case sc.W:
		r = "w"
	}
	return sc.Area + t + ":" + r
}

// ParseAllow reads an allow field: a preset name or a list of scopes. It
// returns the scopes in their written form. A nil raw gives the default.
func ParseAllow(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return DefaultAllow, nil
	}
	var preset string
	if json.Unmarshal(raw, &preset) == nil {
		if p, ok := allowPresets[preset]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("%q isn't a preset. allow is \"member\", \"viewer\" or a list of scopes", preset)
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return nil, fmt.Errorf(`allow is "member", "viewer" or a list of scopes like ["shared/status.*:w"]`)
	}
	return NormalizeAllow(list)
}

// NormalizeAllow checks a list of scopes, expanding presets given as items.
func NormalizeAllow(list []string) ([]string, error) {
	var out []string
	for _, s := range list {
		if p, ok := allowPresets[s]; ok {
			out = append(out, p...)
			continue
		}
		sc, err := ParseScope(s)
		if err != nil {
			return nil, err
		}
		out = append(out, sc.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("allow needs at least one scope")
	}
	if len(out) > MaxScopes {
		return nil, fmt.Errorf("at most %d scopes", MaxScopes)
	}
	return out, nil
}

// Op is what a request wants to do.
type Op struct {
	Area    string // "shared", "users" or "me"
	Key     string // the key, or the prefix of a pattern
	Pattern bool
	Write   bool
}

// Covers reports whether the scope allows op. A key covers only itself; a
// prefix p covers keys and patterns starting with p.
func (sc Scope) Covers(op Op) bool {
	if sc.Area != "*" && sc.Area != op.Area {
		return false
	}
	if op.Write && !sc.W || !op.Write && !sc.R {
		return false
	}
	if !sc.Pattern {
		return !op.Pattern && op.Key == sc.Prefix
	}
	return strings.HasPrefix(op.Key, sc.Prefix)
}

// Grant is a visitor's rights in one space.
type Grant struct {
	ID     string
	Owner  bool
	Allow  []string
	Scopes []Scope
	Source string
	Until  int64 // ms, 0 for no end
}

func (g Grant) Live(now time.Time) bool { return g.Until == 0 || g.Until > now.UnixMilli() }

// Rights is what one visitor holds in one space.
type Rights struct {
	Settings Settings
	Owner    bool
	Grants   []Grant // live grants only
}

// CanRead reports whether the space shows itself to this visitor at all:
// readable by anyone, or the visitor holds some grant. A write-only grant
// still makes the space visible, so a sensor's writes don't count as misses.
func (r *Rights) Visible() bool {
	return r.Settings.Read || r.Owner || len(r.Grants) > 0
}

// Allows applies settings, ownership and grants. The fixed rules (users/{id}
// only by {id}, me/ always your own, writes need a visitor) are the caller's.
func (r *Rights) Allows(op Op) bool {
	if r.Owner {
		return true
	}
	if op.Write && r.Settings.WriteAnyone(op.Area) || !op.Write && r.Settings.Read {
		return true
	}
	for _, g := range r.Grants {
		for _, sc := range g.Scopes {
			if sc.Covers(op) {
				return true
			}
		}
	}
	return false
}

// ParseScopes turns stored allow strings back into scopes. Stored lists were
// checked when written, so a bad entry is skipped.
func ParseScopes(allow []string) []Scope {
	out := make([]Scope, 0, len(allow))
	for _, s := range allow {
		if sc, err := ParseScope(s); err == nil {
			out = append(out, sc)
		}
	}
	return out
}

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
