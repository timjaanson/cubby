# cubby

A small back-end for live web apps. One binary gives you a key-value store with live updates, so a static page gets shared state that everyone sees change as it happens. No accounts, no schema, no server code to write.

It's for the things that are too small for a real back-end: a poll for the group chat, a shared shopping list, a quiz night, a scoreboard, a board several people edit at once, a sensor dashboard, a script that tells another script something changed.

- **Live updates.** Watch a key or a pattern and changes arrive as they happen, over one WebSocket per page or plain HTTP long polling.
- **No sign-up.** Each browser gets an anonymous identity by itself. One request creates a space.
- **Private unless you say so.** Spaces are invite-only. One word (`public: "read"`, `"respond"`, `"all"`) opens them up.
- **Safe with many writers.** Every value has a version, and `ifVersion` refuses a write if someone got there first. Each visitor has an area only they can write, so votes and names can't be faked.
- **Pulses for the fast stuff.** Cursors, drags, reactions and game inputs go out as pulses: values that are never stored, reach whoever is listening right now, and can be sent dozens of times a second.
- **Live primitives.** Expiring keys for presence and locks, a shared server clock, write deadlines, and time series for sensor readings.
- **Plain HTTP underneath.** A browser client at `/kv.js`, a CLI in the same binary, and `curl` for everything else.

## A live poll in 20 lines

```html
<button>Pizza</button> <button>Tacos</button> <p id="results"></p>

<script type="module">
import { kv, createSpace } from "https://cubby.example.com/kv.js"

// One poll per link: the space name lives in the URL fragment.
const space = location.hash.slice(1) || (await createSpace({ public: "respond" })).space
location.hash = space
const db = kv(space, { live: true })

// A vote sits in the voter's own users/ area, so nobody else can change it.
for (const b of document.querySelectorAll("button"))
  b.onclick = () => db.user().set("vote", b.textContent)

// Every open page sees votes the moment they're cast.
const votes = new Map()
db.users.watch("vote", c => c.deleted ? votes.delete(c.user) : votes.set(c.user, c.value), {
  onSync: () => results.textContent = ["Pizza", "Tacos"]
    .map(v => `${v} ${[...votes.values()].filter(x => x === v).length}`).join(", "),
})
</script>
```

Open it, send the link around, and the counts move on every screen as people tap. For something that moves rather than something to keep, like everyone's cursor, send pulses: `db.user().pulse("cursor", { x, y })` on every mouse move, and `db.users.onPulse("cursor", p => draw(p.user, p.value))` on every screen. A page that only needs to save something is shorter still: `await db.shared.set("list", items)` and `await db.shared.get("list")`.

## Install and run

With Go 1.26 or newer:

```sh
go install github.com/timjaanson/cubby@latest
cubby serve
```

Or build static binaries for Linux and macOS with `./build.sh` (they land in `dist/`).

`cubby serve` listens on port 28229 and keeps its data in the usual place for your OS. No config file is needed. Pages load the client from your server, `https://your-host/kv.js`. To put it on the internet, run it behind a TLS proxy: [docs/deploy.md](docs/deploy.md) has Caddy, nginx and Apache configs and a systemd unit.

## The same from a shell

```sh
$ cubby new notes                          # a private space you own
notes-cedar-pilot-mango-frost
ot_8Hq3vT...                               # owner token: your recovery key, shown once
$ cubby set notes-cedar-pilot-mango-frost/shared/hello "Hello there"
$ cubby watch notes-cedar-pilot-mango-frost/shared/hello      # prints each change as it happens
$ curl "localhost:28229/v1/notes-cedar-pilot-mango-frost/shared/hello?token=$(cubby whoami --show-token)"
Hello there
```

## How data is laid out

```
space                 notes-cedar-pilot-mango-frost   one per app or room
├── shared/{key}      common data
├── users/{id}/{key}  each visitor's public part; only they write it
└── me/{key}          each visitor's private data
```

Keys group with dots (`msg.1791449600123`), and a pattern like `msg.*` lists or watches them all. Values are JSON or text up to 64 KB.

## Docs

- [docs/usage.md](docs/usage.md): the JS client, the CLI and plain HTTP, sharing, series, expiry and limits
- [docs/deploy.md](docs/deploy.md): installing, reverse proxies, systemd, configuration, data and backups
- [docs/scenarios.md](docs/scenarios.md): worked examples, from a shared note to a chat room and a sensor board
- [docs/agents.md](docs/agents.md): a guide for coding agents building pages on cubby
- [docs/spec.md](docs/spec.md): the full contract; [docs/design.md](docs/design.md): why it works this way
- [docs/development.md](docs/development.md): building, tests and code layout

Private spaces keep strangers out, but the server's operator can read everything. Don't store secrets in it.
