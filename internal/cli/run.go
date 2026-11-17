// Package cli is everything behind the cubby command: argument parsing,
// the serve and admin commands, and the HTTP client commands.
package cli

import (
	"fmt"

	"github.com/timjaanson/cubby/internal/version"
)

const usage = `cubby: a key-value store over plain HTTP, server and client in one binary.

Server:
  cubby serve [-p PORT]           run the server, port 28229 unless told (see 'cubby help serve')
  cubby config [--defaults]       print the server config as TOML, to check it or start a file
  cubby admin <command>           look after the server's data (spaces, export, delete, owner, stats, cleanup)

Client (talks to --server or $CUBBY_SERVER, default http://localhost:28229; a bare port means localhost):
  cubby new [label] [--anyone L]  create a space you own; private unless --anyone read|post|all
  cubby spaces                    list the spaces you belong to, newest change first
  cubby get <path>                print a value
  cubby set <path> [value|-]      write a value from the argument or stdin
  cubby del <path>                delete a value
  cubby batch <space> [file|-]    write several keys from a JSON array, all or nothing
  cubby ls <path>                 list a key or pattern, one tab-separated line per item
  cubby watch <path>              print changes as they happen
  cubby update <path>             read, change with --exec or $EDITOR, write back safely
  cubby add <path> [value]        add a point to a series ('space/shared/env.*' a=1 b=2 for several)
  cubby series <path>             print a series' points, oldest first
  cubby pulse <path> [value]      send a pulse: not stored, only for whoever listens now
  cubby pulses <path>             print pulses as they arrive
  cubby limits [space]           show effective limits and usage
  cubby time                     estimate server clock offset and round trip
  cubby name [label]              make up a valid space name, no server needed
  cubby whoami | url <path>

Signing in (the first command that needs it makes this machine's key, ES256):
  cubby token [--ttl 1h]                             print a token for curl: Bearer $(cubby token)
  cubby device backup > backup.json                  a backup key: keep it in a password manager
  cubby login <file | ->                             use a backup key or credentials file here
  cubby api-key new <space> [--until 30d|never]      static key for webhooks and tools that can't sign

Sharing (levels: read < post < all < owner; banned keeps someone out):
  cubby invite new <space> [--access L] [--link URL] mint an invite: access all, 10 uses, 7 days
  cubby join <space> <invite or link>                redeem an invite on this machine
  cubby access <space> | settings <space> | invite ls|set|rm | member ls|set|rm
  cubby drop <space>

Your devices (your visitor on another machine, not sharing):
  cubby device pair [--ttl 10m]                      one-use code for another device, then wait
  cubby device accept <code or link>                 join the visitor that made the code
  cubby device ls | name <label> | rm <id> | rm --others

Paths mirror the URL after /v1/:
  poll-cedar-pilot-mango/shared/question     poll-cedar-pilot-mango/by/me/vote
  'poll-cedar-pilot-mango/shared/msg.*'      'poll-cedar-pilot-mango/by/*/vote'
  poll-cedar-pilot-mango/to/me/draft         poll-cedar-pilot-mango/to/owners/answer
  'poll-cedar-pilot-mango/inbox/*/answer'    'poll-cedar-pilot-mango/inbox/owners/*/answer'

Run 'cubby help <command>' for flags, or see the README.
`

// run dispatches a command line and returns the exit code.
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	var code int
	switch cmd {
	case "serve":
		code = cmdServe(rest)
	case "admin":
		code = cmdAdmin(rest)
	case "config":
		code = cmdConfig(rest)
	case "version", "--version", "-v":
		fmt.Println("cubby", version.Version)
	case "help", "--help", "-h":
		if len(rest) > 0 {
			code = cmdHelp(rest[0])
		} else {
			fmt.Print(usage)
		}
	default:
		code = runClient(cmd, rest)
	}
	return code
}
