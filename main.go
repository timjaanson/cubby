// cubby is a self-hosted key-value store over plain HTTP. The same binary
// runs the server (cubby serve), administers its data (cubby admin) and
// acts as a command-line client (everything else).
package main

import (
	"os"

	"github.com/timjaanson/cubby/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
