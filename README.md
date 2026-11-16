# cubby

A small back-end for live web apps. One binary serves a key-value store with live updates, so a static page gets shared state that everyone sees change as it happens. No accounts, no schema, no server code.

- **No sign-up.** Each browser makes its own key pair and signs short-lived tokens with it. Nothing permanent crosses the wire.
- **Private by default.** A space is invite-only. Everyone in it has one level: `read`, `post` (their own keys), `all` (common keys too) or `owner`.
- **Live.** Watch a key or a pattern over one WebSocket per page, or plain HTTP long polling. Pulses carry cursors and other fast, unstored values.
- **Safe with many writers.** Every write has a version, and `ifVersion` refuses a write if someone got there first.
- **Plain HTTP underneath.** A browser client at `/kv.js`, a CLI in the same binary, `curl` for the rest.

## A live poll

```html
<button>Pizza</button> <button>Tacos</button> <p id="results"></p>

<script type="module">
import { kv, createSpace } from "https://cubby.example.com/kv.js"

const space = location.hash.slice(1) || (await createSpace({ anyone: "post" })).space
location.hash = space
const db = kv(space, { live: true })

for (const b of document.querySelectorAll("button"))
  b.onclick = () => db.mine.set("vote", b.textContent)    // your own key: nobody else can change it

const votes = new Map()
db.by.watch("vote", c => c.deleted ? votes.delete(c.by) : votes.set(c.by, c.value), {
  onSync: () => results.textContent = ["Pizza", "Tacos"]
    .map(v => `${v} ${[...votes.values()].filter(x => x === v).length}`).join(", "),
})
</script>
```

## Run it

```sh
go install github.com/timjaanson/cubby@latest    # Go 1.26+, or ./build.sh for static binaries
cubby serve                                      # port 28229
```

```sh
$ cubby new notes                                 # a private space you own
notes-cedar-pilot-mango-frost
$ cubby set notes-cedar-pilot-mango-frost/shared/hello "Hello there"
$ curl -H "Authorization: Bearer $(cubby token)" localhost:28229/v1/notes-cedar-pilot-mango-frost/shared/hello
Hello there
```

## Docs

- [docs/usage.md](docs/usage.md): the JS client, the CLI and curl. Start here, person or coding agent.
- [docs/api.md](docs/api.md): the HTTP API, for clients in other languages.
- [docs/deploy.md](docs/deploy.md): running it on a server.
- [docs/development.md](docs/development.md): building and testing.

Private spaces keep strangers out, but the server's operator can read everything. Don't store secrets in it.
