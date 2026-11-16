// Package apierr defines the API's error type and the errors shared by the
// store and the HTTP layer. Each one carries the HTTP status and the stable
// code clients see in the JSON body.
package apierr

import (
	"fmt"
	"math"
	"time"
)

type Error struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

var (
	NotFound         = &Error{Status: 404, Code: "not_found", Message: "No such key."}
	ServerFull       = &Error{Status: 507, Code: "server_full", Message: "The server is full."}
	MissingToken     = &Error{Status: 401, Code: "missing_token", Message: "This request needs a token: a JWT signed by a registered device key, sent as Authorization: Bearer <token>. Register a key with POST /v1/visitors."}
	UnknownKey       = &Error{Status: 401, Code: "unknown_key", Message: "No device has this key: it was never registered, or was signed out. Register it with POST /v1/visitors, or add it to a visitor with POST /v1/pair."}
	InvalidAPIKey    = &Error{Status: 401, Code: "invalid_api_key", Message: "This API key is unknown, expired or revoked."}
	APIKeyNotAllowed = &Error{Status: 403, Code: "api_key_not_allowed", Message: "An API key reads and writes in its space only. This needs a token signed by a device key."}

	// NoSpace is the answer for a space that doesn't exist and for a private
	// space the request can't read. They must stay identical, so the
	// message names both causes.
	NoSpace = &Error{Status: 404, Code: "no_space",
		Message: "No such space, or it's private and this visitor hasn't joined it. Redeem an invite with POST /v1/{space}/join, or check the name."}
	// InviteInvalid is every failed join, whatever was wrong.
	InviteInvalid = &Error{Status: 404, Code: "invite_invalid",
		Message: "This invite is unknown, expired, used up or revoked, or the space doesn't exist. Ask the space's owner for a new one."}
	OwnerOnly = &Error{Status: 403, Code: "owner_only", Message: "Only an owner of this space can do this."}
)

// TokenError is a token that was refused: code says why, as the auth
// package decided.
// A token refused for its times carries serverTime, so a client with a
// wrong clock sees it at once.
func TokenError(code, msg string) *Error {
	e := &Error{Status: 401, Code: code, Message: msg}
	if code == "token_expired" || code == "token_not_yet_valid" {
		e.Extra = map[string]any{"serverTime": time.Now().UnixMilli()}
	}
	return e
}

// APIKeySpace is an API key used on a space it isn't for.
func APIKeySpace(space string) *Error {
	return &Error{Status: 403, Code: "api_key_space", Message: "This API key is for the space " + space + " only."}
}

// NoAccess is a write that takes a higher level than the visitor has. need
// is the level it takes: post for by/ and to/, all for shared/.
func NoAccess(need fmt.Stringer) *Error {
	return &Error{Status: 403, Code: "no_access", Extra: map[string]any{"need": need.String()},
		Message: fmt.Sprintf("Writing here takes access %q or higher. An owner can give it, with an invite or by setting this visitor's level.", need.String())}
}

// NotYours is a write under another visitor's by/{id}/.
func NotYours(id string) *Error {
	return &Error{Status: 403, Code: "not_yours", Message: "Only that visitor can write under by/" + id + "/."}
}

func InvalidSettings(msg string) *Error {
	return &Error{Status: 400, Code: "invalid_settings", Message: msg}
}

// InvalidSpace is a space name that breaks the naming rule.
func InvalidSpace(suggestion string) *Error {
	return &Error{Status: 400, Code: "invalid_space",
		Message: "A space name is 16 to 63 lowercase letters, digits and single hyphens, like poll-cedar-pilot-mango-frost.",
		Extra:   map[string]any{"suggestion": suggestion}}
}

// SpaceExists answers a request to create a space under a name that's taken.
func SpaceExists(name string) *Error {
	return &Error{Status: 409, Code: "space_exists", Extra: map[string]any{"space": name},
		Message: name + " already exists. Pick another name, or leave the name out to get a fresh one."}
}

func Quota(code, msg string, limit, used int64) *Error {
	return &Error{Status: 507, Code: code, Message: msg, Extra: map[string]any{"limit": limit, "used": used}}
}

func BadParam(name, msg string) *Error {
	return &Error{Status: 400, Code: "invalid_parameter", Message: msg, Extra: map[string]any{"parameter": name}}
}

func RateLimited(code string, retry time.Duration) *Error {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	msg := "Too many requests. Try again in a moment."
	switch code {
	case "space_limit":
		msg = "Too many new spaces. Try again later."
	case "stored_limit":
		msg = "This address has added as much stored data today as it may. Deleting frees space but not this budget. Try again later."
	case "miss_limit":
		msg = "Too many different spaces that don't exist from this address. Spaces you used in the last hour keep working. Try others later."
	}
	return &Error{Status: 429, Code: code, Message: msg, Extra: map[string]any{"retryAfter": secs}}
}

func IsSeries(key string) *Error {
	return &Error{Status: 409, Code: "is_series", Extra: map[string]any{"key": key},
		Message: key + " is a series. Add points with POST " + key + "/series, or DELETE it first."}
}

func NotSeries(key string) *Error {
	return &Error{Status: 409, Code: "not_series", Extra: map[string]any{"key": key},
		Message: key + " is a plain key, not a series. Read it with GET " + key + ", replace it with PUT."}
}

// Banned answers every request a banned visitor makes in the space. until is
// in ms, 0 for a ban without end.
func Banned(until int64) *Error {
	var u any
	if until != 0 {
		u = until
	}
	return &Error{Status: 403, Code: "banned", Message: "This visitor is banned from this space.", Extra: map[string]any{"until": u}}
}

// NotSender is a write to someone else's to/ slot.
var NotSender = &Error{Status: 403, Code: "not_yours",
	Message: "Only the sender writes their to/ slot. A recipient can delete what was sent to them under inbox/."}
