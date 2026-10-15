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
	SpaceLocked  = &Error{Status: 403, Code: "space_locked", Message: "This space is locked. Writing shared/ needs the right Space-Key header."}
	ServerFull   = &Error{Status: 507, Code: "server_full", Message: "The server is full."}
	MissingToken = &Error{Status: 401, Code: "missing_token", Message: "This request needs a token. Get one with POST /v1/anon and send it as Authorization: Bearer <token>."}
	InvalidToken = &Error{Status: 401, Code: "invalid_token", Message: "The token is unknown or expired. Get a new one with POST /v1/anon."}
)

// InvalidSpace is a space name that breaks the naming rule.
func InvalidSpace(suggestion string) *Error {
	return &Error{Status: 400, Code: "invalid_space",
		Message: "A space name is an optional label plus 8 random characters with at least 2 digits and 2 letters, like poll-k7m2x9q4.",
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
	if code == "space_limit" {
		msg = "Too many new spaces. Try again later."
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

var NoSpace = &Error{Status: 404, Code: "not_found", Message: "No such space."}
