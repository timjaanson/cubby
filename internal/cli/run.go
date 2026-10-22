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
  cubby admin <command>           look after the server's data (spaces, export, delete, owner-token, stats, cleanup)

Client (talks to --server or $CUBBY_SERVER, default http://localhost:28229; a bare port means localhost):
  cubby new [label] [--public P]  create a space you own; private unless --public read|respond|all
  cubby get <path>                print a value
  cubby set <path> [value|-]      write a value from the argument or stdin
  cubby del <path>                delete a value
  cubby ls <path>                 list a key or pattern, one tab-separated line per item
  cubby watch <path>              print changes as they happen
  cubby update <path>             read, change with --exec or $EDITOR, write back safely
  cubby add <path> [value]        add a point to a series ('space/shared/env.*' a=1 b=2 for several)
  cubby series <path>             print a series' points, oldest first
  cubby limits [space]           show effective limits and usage
  cubby time                     estimate server clock offset and round trip
  cubby name [label]              make up a valid space name, no server needed
  cubby whoami [--show-token] | url <path>

Sharing (owners invite, others join):
  cubby token new <space> [--label L] [--link URL]   mint an invite: member, 10 uses, 7 days
  cubby join <space> <token or link>                 redeem an invite on this machine
  cubby access <space> | settings <space> | token ls|set|rm | grant ls|add|rm | owner-token | drop

Paths mirror the URL after /v1/:
  poll-cedar-pilot-mango/shared/question     poll-cedar-pilot-mango/users/me/vote
  'poll-cedar-pilot-mango/shared/msg.*'      'poll-cedar-pilot-mango/users/*/vote'
  poll-cedar-pilot-mango/me/draft

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
