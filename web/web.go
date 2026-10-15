// Package web holds browser-side assets that the server embeds and serves.
package web

import (
	"bytes"
	_ "embed"
	"strings"

	"github.com/timjaanson/cubby/internal/names"
)

//go:embed kv.js
var kvSource []byte

// KVJS is the JavaScript client, served at /kv.js, with the name word list
// filled in.
var KVJS = bytes.Replace(kvSource, []byte("__CUBBY_WORDS__"), []byte(strings.Join(names.Words, " ")), 1)
