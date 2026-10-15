// Package web holds browser-side assets that the server embeds and serves.
package web

import _ "embed"

// KVJS is the JavaScript client, served at /kv.js.
//
//go:embed kv.js
var KVJS []byte
