// Package apierr defines the API's error type and the errors shared by the
// store and the HTTP layer. Each one carries the HTTP status and the stable
// code clients see in the JSON body.
package apierr

import (
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
	NotFound     = &Error{Status: 404, Code: "not_found", Message: "No such key."}
	ServerFull   = &Error{Status: 507, Code: "server_full", Message: "The server is full."}
	MissingToken = &Error{Status: 401, Code: "missing_token", Message: "This request needs a visitor token. Get one with POST /v1/anon and send it as Authorization: Bearer <token>."}
	InvalidToken = &Error{Status: 401, Code: "invalid_token", Message: "The visitor token is unknown or expired. Get a new one with POST /v1/anon; it starts with no grants."}

	// NoSpace is the answer for a space that doesn't exist and for a private
	// space the request can't read. They must stay identical, so the
	// message names both causes.
	NoSpace = &Error{Status: 404, Code: "no_space",
		Message: "No such space, or it's private and this visitor hasn't joined it. Redeem an invite with POST /v1/{space}/join, or check the name."}
	// InviteInvalid is every failed join, whatever was wrong.
	InviteInvalid = &Error{Status: 404, Code: "invite_invalid",
		Message: "This invite is unknown, expired, used up or revoked, or the space doesn't exist. Ask the space's owner for a new one."}
	NoAccess = &Error{Status: 403, Code: "no_access",
		Message: "This visitor may not write here. The space's owner can invite them with more rights, or change who may write."}
	OwnerOnly = &Error{Status: 403, Code: "owner_only", Message: "Only an owner of this space can do this."}
)

// NotYours is a write under another visitor's users/{id}/.
func NotYours(id string) *Error {
	return &Error{Status: 403, Code: "not_yours", Message: "Only that visitor can write under users/" + id + "/."}
}

func InvalidSettings(msg string) *Error {
	return &Error{Status: 400, Code: "invalid_settings", Message: msg}
}

func InvalidScope(msg string) *Error {
	return &Error{Status: 400, Code: "invalid_scope", Message: msg}
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
