# cubby client scenarios

Five complete pages, a set of shell scripts and a microcontroller sketch. Each page is a single HTML file with all the client code it needs. The server is assumed to run at `https://kv.example.com`.

| Scenario | Settings | Client | Shows |
|---|---|---|---|
| [1. Shared note](#1-shared-note-raw-http) | `public: all`: a group edits one thing | raw `fetch`, plus `curl` | creating a space, reads without a token, getting a token for writes, conditional writes, delete, watching one key |
| [2. Lunch poll](#2-lunch-poll-js-client) | `public: respond`: the author writes `shared/`, everyone votes | JS client | settings per area, votes in `users/`, reading across visitors, private `me/`, inviting a co-author |
| [3. Chat rooms](#3-chat-rooms-js-client) | `public: all`: a space per link | JS client | `createSpace` per room, the creator as owner, messages in `users/`, names, presence, `update` on shared settings |
| [4. Home server status and deploys](#4-home-server-status-and-deploys-shell) | `public: read`: granted machines write | CLI, plus `curl` versions | access tokens for machines, a fixed identity for CI, publishing from cron, reacting with `watch --exec` |
| [5. Room sensor](#5-room-sensor-raw-http-from-a-microcontroller) | `public: read`: the board writes | raw HTTP from C++ and `fetch` | a device redeeming once, series, adding to several in one request, live values, a 24-hour chart |
| [6. Family list](#6-family-list-private-space-invite-links) | private, the default | JS client | `db.open()`, invite links with `db.share()`, the share panel, revoking a person |

[Other situations](#other-situations) at the end sketches more pages and which settings and areas each would use.

## 1. Shared note (raw HTTP)

One text note anyone with the page can edit. Spaces are private by default, so this one is created with `public: all` on purpose: anyone who knows its name may read and write. If two people edit at once, the second save is refused instead of silently overwriting the first. Edits from others appear live.

The only setup is creating the space once:

```
$ curl -s -X POST https://kv.example.com/v1/spaces -d '{"name": "note-hinge-cocoa-orbit-maple", "public": "all"}'
{"space":"note-hinge-cocoa-orbit-maple","owner":"ot_Zr7c...","id":"u_d4h8k2m6p0r4t8w2","token":"vt_Xm3...","settings":{"public":"all",...}}
```

Keep `ot_Zr7c...`. It's how you'd get back in to clean up spam or lock the note down later. The `vt_` is the identity this `curl` call created, which you can throw away.

### The page

```html
<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>Shared note</title>
<style>
  body { font: 16px system-ui, sans-serif; max-width: 40rem; margin: 2rem auto; padding: 0 1rem; }
  textarea { width: 100%; height: 14rem; font: inherit; box-sizing: border-box; }
  #status { color: #666; margin-left: .5rem; }
</style>

<h1>Shared note</h1>
<textarea id="note" placeholder="Write something everyone can see"></textarea>
<button id="save">Save</button>
<span id="status"></span>

<script type="module">
const API = "https://kv.example.com/v1"
const NOTE = `${API}/note-hinge-cocoa-orbit-maple/shared/note`

const $note = document.querySelector("#note")
const $save = document.querySelector("#save")
const $status = document.querySelector("#status")

let token = localStorage.getItem("note-token")
let noteVersion = 0   // version of the note text we have, used for ifVersion
let cursor = 0        // where the next watch continues from (not the same thing)
let dirty = false     // true while the user has unsaved typing

// ---------- writing needs a token ----------

async function write(method, url, body) {
  if (!token) await getToken()
  let res = await send()
  if (res.status === 401) {            // token expired: get a new one and retry once
    await getToken()
    res = await send()
  }
  return res

  function send() {
    return fetch(url, { method, body, headers: { Authorization: `Bearer ${token}` } })
  }
}

async function getToken() {
  const res = await fetch(`${API}/anon`, { method: "POST" })   // a visitor token, vt_...
  if (!res.ok) throw new Error(`Could not get a token (${res.status})`)
  token = (await res.json()).token
  localStorage.setItem("note-token", token)
}

async function save() {
  const text = $note.value
  // An empty body isn't a valid value, so clearing the note means deleting it.
  const res = text === ""
    ? await write("DELETE", `${NOTE}?ifVersion=${noteVersion}`)
    : await write("PUT", `${NOTE}?ifVersion=${noteVersion}`, text)

  if (res.status === 404) {            // clearing a note that was never saved
    dirty = false
    $status.textContent = "Nothing to clear"
    return
  }

  const body = await res.json()

  if (res.status === 412) {
    // Someone saved after we loaded. Keep the user's text and let them decide.
    noteVersion = body.currentVersion
    $status.textContent = "Someone else saved first. Save again to replace their version."
    return
  }
  if (!res.ok) {
    $status.textContent = body.message
    return
  }

  noteVersion = text === "" ? 0 : body.version
  dirty = false
  $status.textContent = "Saved"
}

// ---------- reading needs nothing ----------

async function watch() {
  for (;;) {
    try {
      // cursor 0 returns the current note right away. Later calls wait for changes.
      const res = await fetch(`${NOTE}/watch?version=${cursor}`)

      if (res.status === 410) {        // too old, start over from a snapshot
        cursor = 0
        continue
      }
      if (res.status === 429) {
        const { retryAfter } = await res.json()
        await sleep(retryAfter * 1000)
        continue
      }
      if (!res.ok) throw new Error(`watch failed (${res.status})`)

      const data = await res.json()
      for (const change of data.changes) {
        if (dirty) {
          // Don't overwrite what the user is typing. Their save will hit a conflict.
          $status.textContent = "Someone else changed the note while you were typing"
          continue
        }
        $note.value = change.deleted ? "" : show(change.value)
        noteVersion = change.deleted ? 0 : change.version
        $status.textContent = ""
      }
      cursor = data.version
    } catch (err) {
      console.warn(err)
      await sleep(3000)
    }
  }
}

// Text values arrive as strings. If someone stored JSON, show it as JSON.
const show = value => typeof value === "string" ? value : JSON.stringify(value)
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))

$note.addEventListener("input", () => { dirty = true })
$save.addEventListener("click", save)
watch()
</script>
```

### Things this page has to get right

- **Reading needs no token.** Visitors who only read never call `/anon`. The first save gets one. It's the only credential this page ever sends.
- **Two different versions.** `noteVersion` is the note's own version and goes into `ifVersion`. `cursor` is the `version` from a watch response and is only a position for the next watch. After a timeout, `cursor` can be far ahead of `noteVersion`, because other keys in the space changed. Using it as `ifVersion` would make every save fail.
- **No initial GET.** A watch with `version=0` answers at once with the current note, so loading and watching are one loop.
- **An empty note is a delete.** `PUT` rejects an empty body.
- **The body goes as is.** Whatever is typed is stored exactly and comes back unchanged.

### The same thing with curl

```
$ API=https://kv.example.com/v1
$ N=$API/note-hinge-cocoa-orbit-maple/shared/note

$ curl -s $N
{"error":"not_found","message":"No such key"}

$ curl -s -X POST $API/anon
{"token":"vt_4Jx9cQ2...","id":"u_k7m2x9q4p1r8t3w6"}
$ T=vt_4Jx9cQ2...

$ curl -s -X PUT -H "Authorization: Bearer $T" --data-binary 'Buy milk' $N
{"version":1791449600001}

$ curl -i $N
HTTP/2 200
content-type: text/plain; charset=utf-8
etag: "1791449600001"

Buy milk

$ curl -s -X PUT -H "Authorization: Bearer $T" --data-binary 'Buy oat milk' "$N?ifVersion=1791449600000"
{"error":"version_conflict","message":"Key has changed since version 1791449600000","currentVersion":1791449600001}

$ curl -s -X PUT -H "Authorization: Bearer $T" --data-binary 'Buy oat milk' "$N?ifVersion=1791449600001"
{"version":1791449600002}

$ curl -s "$N/watch?version=1791449600002"
    ... waits until someone else saves ...
{"changes":[{"key":"note","value":"Buy oat milk and bread","version":1791449600005}],"version":1791449600005,"more":false}

$ curl -s -X DELETE -H "Authorization: Bearer $T" $N
{"version":1791449600006}
```

Use `--data-binary`, not `-d`. `-d` strips newlines from the body.

## 2. Lunch poll (JS client)

The poll's question and options live in `shared/`, which only the author writes. Each visitor's vote is in their own `users/` area, where nobody else can change it. A visitor's display preference is in private `me/`.

### One-time setup by the author

```
$ API=https://kv.example.com/v1
$ curl -s -X POST $API/spaces -d '{"label": "lunch-poll", "public": "respond"}'
{"space":"lunch-poll-orbit-lilac-tango-slate","owner":"ot_Hq3...keep this private","id":"u_k7m2x9q4p1r8t3w6",
 "token":"vt_Pw8...","settings":{"public":"respond","read":"anyone","write":{"shared":"granted","users":"anyone","me":"anyone"}}}
$ T=vt_Pw8...

$ curl -s -X PUT -H "Authorization: Bearer $T" \
    --data-binary '{"question":"Lunch today?","options":["pizza","tacos","sushi"]}' \
    $API/lunch-poll-orbit-lilac-tango-slate/shared/poll
{"version":1791449600001}
```

`$T` is now an owner of the space, so it can write `shared/`. Keep it to change the poll later, and keep the owner token in case you lose `$T`. Open pages update live because they watch the `poll` key.

To let a colleague edit the poll too, mint an access token for her. Without options it's a full member, redeemable by 10 visitors within 7 days. One use is enough here:

```
$ curl -s -X POST -H "Authorization: Bearer $T" $API/lunch-poll-orbit-lilac-tango-slate/tokens \
    -d '{"label": "maria", "uses": 1}'
{"id":"t_3k9f2m7q","token":"at_Lk2...","label":"maria","allow":["*:rw"],"uses":1,"until":1792054400000,"grantFor":null}
```

She redeems it once from her own machine, and from then on her CLI identity may write `shared/`:

```
$ cubby join lunch-poll-orbit-lilac-tango-slate at_Lk2...
*:rw
$ cubby set lunch-poll-orbit-lilac-tango-slate/shared/poll '{"question":"Lunch Friday?","options":["pizza","ramen"]}'
```

### The page

```html
<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>Lunch poll</title>
<style>
  body { font: 16px system-ui, sans-serif; max-width: 30rem; margin: 2rem auto; padding: 0 1rem; }
  #options button { display: flex; justify-content: space-between; width: 100%;
                    margin: .4rem 0; padding: .7rem 1rem; font: inherit; cursor: pointer; }
  #options button.mine { outline: 2px solid #2a6df4; }
  #error { color: #b00020; }
</style>

<h1 id="question">Loading</h1>
<div id="options"></div>
<p id="total"></p>
<label><input type="checkbox" id="percent"> Show percentages</label>
<p id="error"></p>

<script type="module">
import { kv } from "https://kv.example.com/kv.js"

const db = kv("lunch-poll-orbit-lilac-tango-slate")

const $question = document.querySelector("#question")
const $options = document.querySelector("#options")
const $total = document.querySelector("#total")
const $percent = document.querySelector("#percent")
const $error = document.querySelector("#error")

const myId = await db.id()
let poll                  // undefined while loading, null if not set up
const votes = new Map()   // visitor ID -> option

const showError = err => { $error.textContent = err.message }

// The poll itself. Delivered once at start, then whenever the author changes it.
// onSync comes after each batch of changes, so the page renders once per batch.
// The first one (initial) means the snapshot is complete: no poll by then means none is set up.
db.shared.watch("poll", change => {
  poll = change.deleted ? null : change.value
}, {
  onSync: ({ initial }) => {
    if (initial && poll === undefined) poll = null
    render()
  },
  onError: showError,
})

// Everyone's vote, read from every visitor's area.
db.users.watch("vote", change => {
  if (change.deleted) votes.delete(change.user)
  else votes.set(change.user, change.value)
}, { onSync: render, onError: showError })

$percent.checked = (await db.me.get("showPercent")) ?? false
$percent.addEventListener("change", async () => {
  render()
  await db.me.set("showPercent", $percent.checked)
})

async function vote(option) {
  $error.textContent = ""
  const previous = votes.get(myId)

  // Show the vote at once. The watch confirms it a moment later.
  if (option === null) votes.delete(myId)
  else votes.set(myId, option)
  render()

  try {
    if (option === null) await db.user().delete("vote")
    else await db.user().set("vote", option)
  } catch (err) {
    if (previous === undefined) votes.delete(myId)
    else votes.set(myId, previous)
    render()
    $error.textContent = err.code === "rate_limited"
      ? "Slow down a little and try again."
      : err.message
  }
}

function render() {
  if (!poll || !Array.isArray(poll.options)) {
    $question.textContent = poll === null ? "This poll isn't set up yet" : "Loading"
    $options.replaceChildren()
    $total.textContent = ""
    return
  }

  const counts = Object.fromEntries(poll.options.map(option => [option, 0]))
  let total = 0
  for (const option of votes.values()) {
    if (option in counts) { counts[option]++; total++ }   // ignore votes for options that don't exist
  }
  const mine = votes.get(myId)

  $question.textContent = poll.question
  $options.replaceChildren(...poll.options.map(option => {
    const button = document.createElement("button")
    const label = document.createElement("span")
    const count = document.createElement("strong")
    label.textContent = option
    count.textContent = $percent.checked
      ? `${total ? Math.round(100 * counts[option] / total) : 0}%`
      : counts[option]
    button.append(label, count)
    button.className = option === mine ? "mine" : ""
    button.onclick = () => vote(option === mine ? null : option)   // click again to remove
    return button
  }))
  $total.textContent = `${total} vote${total === 1 ? "" : "s"}`
}
</script>
```

### Notes

- **Nothing a visitor does can change the poll or anyone else's vote.** `shared/` is writable only by owners and grant holders, and `users/` only accepts writes from each area's own visitor.
- **Votes need no invite.** `write.users` is `anyone`, so every visitor can vote. An embedded access token in the page would protect nothing, since anyone could read it from the source.
- **Votes for options that don't exist are ignored.** A visitor can write anything into their own `vote`, so the page shouldn't trust it blindly.
- **One vote per visitor, not per person.** Clearing site data creates a new visitor who can vote again. Anonymous identities can't prevent that.
- **This page gets a token on load,** because it needs the visitor's ID to highlight their vote and reads `me/`. A page that only displayed results wouldn't need one.

## 3. Chat rooms (JS client)

Each room is its own space, created with `public: all` because the page's idea is "anyone with the link can join". For a room among friends only, leave `public` out and add invite links as in [scenario 6](#6-family-list-private-space-invite-links). Opening the page without a room creates one and puts its name in the address bar, so the URL is the invite. The browser that created the room owns it and can delete anyone's messages. Messages and names live in each visitor's `users/` area, so only their author can change them. The room's title is shared and safely editable by anyone. "Here now" comes from timestamps visitors refresh while the page is open.

```html
<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>Chat</title>
<style>
  body { font: 16px system-ui, sans-serif; max-width: 38rem; margin: 2rem auto; padding: 0 1rem; }
  h1 { cursor: pointer; margin-bottom: .2rem; }
  #here { color: #2a7d2a; font-size: 14px; }
  #invite { font-size: 14px; margin: 1rem 0; }
  #invite input { width: 100%; font: 13px monospace; }
  form { display: grid; grid-template-columns: 10rem 1fr auto; gap: .5rem; margin-bottom: 1rem; }
  input, button { font: inherit; }
  .msg { border-top: 1px solid #ddd; padding: .5rem 0; }
  .meta { color: #666; font-size: 13px; }
  .meta button { margin-left: .5rem; font-size: 12px; }
  #error { color: #b00020; }
</style>

<h1 id="title" title="Click to rename">Chat</h1>
<div id="here"></div>
<label id="invite">Invite link <input id="link" readonly></label>

<form id="form">
  <input id="name" placeholder="Your name" maxlength="40">
  <input id="text" placeholder="Message" maxlength="1000" autocomplete="off" required>
  <button>Send</button>
</form>
<p id="error"></p>

<div id="messages"></div>

<script type="module">
import { kv, createSpace } from "https://kv.example.com/kv.js"

const SHOW = 100
const HERE_FOR = 30_000      // someone counts as here if seen in the last 30 s

const $ = selector => document.querySelector(selector)
const showError = err => { $("#error").textContent = err.message }

// The room is the space name in the URL fragment. No room yet: make one.
// The creator owns it. The owner token is a recovery key this page doesn't need.
let room = new URLSearchParams(location.hash.slice(1)).get("room")
if (!room) {
  room = (await createSpace({ label: "room", public: "all" })).space   // anyone with the link
  history.replaceState(null, "", `#room=${room}`)
}
$("#link").value = location.href

const db = kv(room)
const myId = await db.id()
const isOwner = (await db.open()).owner     // the room's creator may delete any message

const messages = new Map()   // "user/key" -> { user, key, text }
const names = new Map()      // visitor ID -> display name
const seen = new Map()       // visitor ID -> last heartbeat time
let settings = { title: "Chat" }

// ---------- loading ----------

// The newest messages from everyone. The list's version is where watching continues.
const { items, version } = await db.users.list("msg.*", { order: "desc", limit: SHOW })
for (const item of items) setUserKey(item)

// Everything else is read after the list, so the watches below cover later changes.
for (const item of (await db.users.list("name")).items) setUserKey(item)
for (const item of (await db.users.list("seen")).items) setUserKey(item)
settings = (await db.shared.get("settings")) ?? settings
$("#name").value = (await db.user().get("name")) ?? ""
render()

// One watch for every visitor's keys (messages, names, presence), one for the settings.
// Both continue from the first list's version, so nothing written since is missed.
// Heartbeats arrive in bursts, so render once per batch, in onSync.
db.users.watch("*", change => {
  if (change.deleted) deleteUserKey(change)
  else setUserKey(change)
}, { from: version, onSync: render, onError: showError })

db.shared.watch("settings", change => {
  if (!change.deleted) settings = change.value
}, { from: version, onSync: render, onError: showError })

function setUserKey({ user, key, value }) {
  if (key === "name") names.set(user, String(value))
  else if (key === "seen" && typeof value === "number") seen.set(user, value)
  else if (key.startsWith("msg.") && typeof value?.text === "string") {
    messages.set(`${user}/${key}`, { user, key, text: value.text })
  }
}

function deleteUserKey({ user, key }) {
  if (key === "name") names.delete(user)
  else if (key === "seen") seen.delete(user)
  else if (key.startsWith("msg.")) messages.delete(`${user}/${key}`)
}

// ---------- presence ----------

// Each open page writes the current time every 20 s. Readers treat anything older
// than 30 s as gone. No expiry on the server needed.
async function heartbeat() {
  if (document.hidden) return
  try {
    await db.user().set("seen", Date.now())
  } catch (err) {
    console.warn(err)
  }
}
heartbeat()
setInterval(heartbeat, 20_000)
setInterval(render, 5_000)                  // so people drop off the list on time
document.addEventListener("visibilitychange", heartbeat)

// ---------- actions ----------

$("#name").addEventListener("change", async () => {
  const name = $("#name").value.trim()
  try {
    if (name) await db.user().set("name", name)
    else await db.user().delete("name")
  } catch (err) {
    showError(err)
  }
})

$("#form").addEventListener("submit", async event => {
  event.preventDefault()
  $("#error").textContent = ""
  const text = $("#text").value.trim()
  if (!text) return

  const time = String(Date.now()).padStart(13, "0")
  const random = Math.random().toString(36).slice(2, 6)
  try {
    await db.user().set(`msg.${time}.${random}`, { text })
    $("#text").value = ""
  } catch (err) {
    $("#error").textContent = {
      rate_limited: "You're sending too fast. Wait a moment.",
      space_quota_exceeded: "This room is full.",
      visitor_quota_exceeded: "You've written as much as one visitor can.",
    }[err.code] ?? err.message
  }
})

$("#title").addEventListener("click", async () => {
  const title = prompt("Room name", settings.title)
  if (!title) return
  try {
    // update() re-reads and retries if someone changed the settings in between,
    // so renaming never wipes out another field someone just set.
    settings = await db.shared.update("settings", current => ({ ...(current ?? {}), title }))
    render()
  } catch (err) {
    showError(err)
  }
})

async function remove(user, key) {
  try {
    await db.user(user).delete(key)      // another visitor's message: allowed for the owner only
  } catch (err) {
    showError(err)
  }
}

// ---------- rendering ----------

const nameOf = id => names.get(id) ?? "anonymous"

function render() {
  const title = typeof settings?.title === "string" ? settings.title : "Chat"
  $("#title").textContent = title
  document.title = title

  const now = Date.now()
  const here = [...seen].filter(([, time]) => now - time < HERE_FOR).map(([id]) => nameOf(id))
  $("#here").textContent = here.length ? `Here now: ${here.join(", ")}` : ""

  // Keys start with the time, so sorting by key sorts by time.
  const newest = [...messages.values()]
    .sort((a, b) => b.key.localeCompare(a.key))
    .slice(0, SHOW)

  $("#messages").replaceChildren(...newest.map(({ user, key, text }) => {
    const div = document.createElement("div")
    div.className = "msg"
    const meta = document.createElement("div")
    meta.className = "meta"
    meta.textContent = `${nameOf(user)} · ${new Date(Number(key.split(".")[1])).toLocaleString()}`
    const body = document.createElement("div")
    body.textContent = text

    if (user === myId || isOwner) {
      const del = document.createElement("button")
      del.textContent = "delete"
      del.onclick = () => remove(user, key)
      meta.append(del)
    }
    div.append(meta, body)
    return div
  }))
}
</script>
```

### Notes

- **The URL is the room.** `#room=room-slate-hinge-cocoa-pilot` is just this page's own convention. The store only sees a space name. Anyone with the link can read and write, since the room was created with `public: all`.
- **Messages belong to their authors.** They live in `users/{id}/msg.*`, so authorship comes from the path and can't be faked. Only the author can change a message. The author and the room's owner can delete it, and the server enforces both.
- **The creator is the owner, on this browser only.** Opening the room elsewhere makes them an ordinary member. For a room that matters, the page could show the owner token from `createSpace` once, to redeem on another device.
- **Names are looked up, not copied.** Messages store only text. Renaming yourself updates all your past messages at once.
- **Presence costs one write every 20 seconds per open page,** and each one wakes everyone's `users/*` watch. Fine for small rooms. Server-side `ttl` is listed under "later" in the spec for when it isn't.
- **List first, then the rest, then watch from the list's version.** Changes in between arrive through the watch. Seeing a change twice is harmless, because handlers set state instead of appending.
- **Text goes in with `textContent`.** Anyone can write anything into their own area, so stored values are untrusted input.
- **Names are per room.** Each room is its own space with its own visitor areas. The visitor ID is the same everywhere, so a page could copy a name between rooms if it wanted to.

## 4. Home server status and deploys (shell)

A home server publishes its health every five minutes. A status page anywhere shows it. A CI job, or you from a laptop, can ask the server to deploy a new version, and a script on the server reacts. It's created with `public: read`, so anyone with the space name can read, and only visitors with a grant, meaning your machines, can write.

Everything here uses the `cubby` binary as a client. A `curl` version of each script follows, for machines without the binary.

### One-time setup

From your laptop, which becomes the owner:

```
$ export CUBBY_SERVER=https://kv.example.com
$ cubby new homelab --public read
homelab-maple-quilt-tundra-cobalt
ot_Zr7cQ2vLm8RtY6pW3nB0kZ7aD5fH1sE9uG2iX4oC6q
```

Put the owner token in your password manager. Then invite the home server, which publishes status and runs deploys:

```
$ cubby token new homelab-maple-quilt-tundra-cobalt --allow 'shared/*:w' --uses 1 --until 1h --label nas
at_Vb6...
```

On the home server, redeem it once. Its CLI saves its own visitor token, which now holds the grant:

```
nas$ export CUBBY_SERVER=https://kv.example.com
nas$ cubby join homelab-maple-quilt-tundra-cobalt at_Vb6...
shared/*:w
```

CI gets an identity that may only request deploys. Redeeming without an identity returns a new visitor token, which becomes a CI secret:

```
$ cubby token new homelab-maple-quilt-tundra-cobalt --allow 'shared/deploy:w' --uses 1 --until 1h --label ci
at_Qz7r...
$ curl -s -X POST -d '{"token": "at_Qz7r..."}' https://kv.example.com/v1/homelab-maple-quilt-tundra-cobalt/join
{"token":"vt_8Rn2...","id":"u_p1r8t3w6k7m2x9q4","owner":false,"allow":["shared/deploy:w"],"until":null,...}
```

Store `vt_8Rn2...` as `CUBBY_TOKEN` in CI. Both access tokens are used up, so leaving them in your shell history does no harm. `cubby grant ls homelab-maple-quilt-tundra-cobalt` shows both machines, and `cubby token rm` on either token's ID cuts that machine off.

### Publishing status from cron

`/usr/local/bin/publish-status`, run every five minutes with `*/5 * * * * /usr/local/bin/publish-status`:

```sh
#!/bin/sh
set -eu
export CUBBY_SERVER=https://kv.example.com
SPACE=homelab-maple-quilt-tundra-cobalt
HOST=$(hostname)

disk=$(df -P / | awk 'NR == 2 { print $5 }' | tr -d '%')
load=$(cut -d ' ' -f 1 /proc/loadavg)
uptime=$(cut -d ' ' -f 1 /proc/uptime)

printf '{"disk": %s, "load": %s, "uptime": %s, "at": %s}' \
  "$disk" "$load" "$uptime" "$(date +%s)" |
  cubby set "$SPACE/shared/status.$HOST" > /dev/null
```

Reading it from anywhere, with no token:

```
$ cubby ls 'homelab-maple-quilt-tundra-cobalt/shared/status.*'
status.nas	{"disk": 41, "load": 0.12, "uptime": 812734.5, "at": 1791449600}
status.pi	{"disk": 77, "load": 1.83, "uptime": 90211.2, "at": 1791449610}

$ curl -s https://kv.example.com/v1/homelab-maple-quilt-tundra-cobalt/shared/status.nas
{"disk": 41, "load": 0.12, "uptime": 812734.5, "at": 1791449600}
```

A status page uses the same URL with `fetch`, or `db.shared.watch("status.*", ...)` with the JS client to update live.

### Reacting to deploy requests

On the server, a long-running watcher, for example as a systemd service:

```sh
#!/bin/sh
# /usr/local/bin/deploy-watcher
set -eu
export CUBBY_SERVER=https://kv.example.com

exec cubby watch homelab-maple-quilt-tundra-cobalt/shared/deploy \
  --from now \
  --state /var/lib/deploy-watcher/version \
  --exec /usr/local/bin/deploy
```

- `--from now` ignores whatever was requested before the watcher was first started.
- `--state` saves the version after each handled change. After a restart or reboot, the watcher continues where it stopped. A request made while it was down still runs once, and an old one never runs twice.

`/usr/local/bin/deploy`, run once per change with the new value on stdin:

```sh
#!/bin/sh
set -eu
SPACE=homelab-maple-quilt-tundra-cobalt

[ "$KV_DELETED" = 1 ] && exit 0          # a cleared request means nothing to do
ref=$(cat)                               # the requested version, like v1.4.2

cubby set "$SPACE/shared/deploy-status" "deploying $ref" > /dev/null

if /opt/app/deploy.sh "$ref" > /var/log/deploy.log 2>&1; then
  cubby set "$SPACE/shared/deploy-status" "deployed $ref" > /dev/null
  cubby update "$SPACE/shared/deploy-count" --exec 'jq "(. // 0) + 1"' > /dev/null
else
  cubby set "$SPACE/shared/deploy-status" "failed $ref" > /dev/null
  exit 1
fi
```

Asking for a deploy, from CI with its own identity, or from your laptop as the owner:

```
$ CUBBY_TOKEN=vt_8Rn2... cubby set homelab-maple-quilt-tundra-cobalt/shared/deploy v1.4.2
1791449600213

$ cubby watch homelab-maple-quilt-tundra-cobalt/shared/deploy-status --from now --raw
deploying v1.4.2
deployed v1.4.2
^C
```

Things to note:

- **Requesting the same version twice does nothing the second time.** A write with identical bytes creates no new version, so the watcher sees no change. To redeploy the same version, make the value unique, for example `v1.4.2 1791449700`.
- **Changes in quick succession collapse.** If two versions are requested while a deploy runs, the watcher sees only the latest one. For deploys, that's what you want.
- **`update` with `jq` is safe with several writers.** If two machines bump `deploy-count` at the same moment, one gets a version conflict and the CLI reruns `jq` on the new value.
- **Exit codes make error handling plain.** `cubby get ... || echo "no status yet"` works because a missing key exits with 1.

### The same with curl

For machines without the binary. These need `curl` and, for watching, `jq`.

Redeeming an access token once and keeping the visitor token it returns:

```sh
curl -fsS -X POST -d '{"token": "at_Vb6..."}' \
  https://kv.example.com/v1/homelab-maple-quilt-tundra-cobalt/join | jq -r .token > ~/.homelab-token
chmod 600 ~/.homelab-token
```

Publishing:

```sh
#!/bin/sh
set -eu
API=https://kv.example.com/v1
SPACE=homelab-maple-quilt-tundra-cobalt
TOKEN=$(cat ~/.homelab-token)

printf '{"disk": %s, "at": %s}' \
  "$(df -P / | awk 'NR == 2 { print $5 }' | tr -d '%')" "$(date +%s)" |
  curl -fsS -X PUT --data-binary @- \
    -H "Authorization: Bearer $TOKEN" \
    "$API/$SPACE/shared/status.$(hostname)" > /dev/null
```

Watching for deploy requests:

```bash
#!/usr/bin/env bash
set -u
API=https://kv.example.com/v1
URL=$API/homelab-maple-quilt-tundra-cobalt/shared/deploy/watch
STATE=/var/lib/deploy-watcher/version

version=$(cat "$STATE" 2> /dev/null || true)      # empty: start from now

while true; do
  body=$(curl -sS -w '\n%{http_code}' "$URL${version:+?version=$version}") || { sleep 5; continue; }
  code=${body##*$'\n'}
  body=${body%$'\n'*}

  case $code in
    200) ;;
    410) version=""; continue ;;                 # too old: continue from now
    429) sleep "$(jq -r .retryAfter <<< "$body")"; continue ;;
    *)   sleep 5; continue ;;
  esac

  jq -r '.changes[] | select(.deleted | not) | .value' <<< "$body" |
    while IFS= read -r ref; do
      /usr/local/bin/deploy-ref "$ref" || echo "deploy of $ref failed" >&2
    done

  version=$(jq -r .version <<< "$body")
  echo "$version" > "$STATE"
done
```

That's what `cubby watch --state --exec` does, written out. The loop is about 25 lines, and most of it handles the cases the CLI handles for you: expired versions, rate limits and network errors.

## 5. Room sensor (raw HTTP from a microcontroller)

An ESP32 board reads temperature, humidity and CO2 every minute and sends all three in one request. A page anywhere shows the current values live and a chart of the last 24 hours. The space is `public: read`: anyone with its name can read the readings, and only the board holds a grant to write them.

### One-time setup

```
$ export CUBBY_SERVER=https://kv.example.com
$ cubby new room --public read
room-slate-hinge-cocoa-pilot
ot_Qm4vT8zK1pW6nR3cY0bH5jL9dF2sA7xE4uG1iO8kZ5
$ cubby token new room-slate-hinge-cocoa-pilot --allow 'shared/env.*:w' --uses 1 --until 1h --label board
at_Hn5...
$ curl -s -X POST -d '{"token": "at_Hn5..."}' https://kv.example.com/v1/room-slate-hinge-cocoa-pilot/join
{"token":"vt_b8Rt2Wq...","id":"u_d4h8k2m6p0r4t8w2","owner":false,"allow":["shared/env.*:w"],"until":null,...}
```

The board gets its own visitor token, with a grant to write `env.*` and nothing else. If the board is lost, `cubby token rm` on the board token's ID revokes it, and its readings stay. A visitor token stays valid as long as it's used at least once a year, which a board posting every minute does.

### The board

```cpp
#include <WiFi.h>
#include <HTTPClient.h>
#include <WiFiClientSecure.h>

const char* URL       = "https://kv.example.com/v1/room-slate-hinge-cocoa-pilot/shared/env.*/series";
const char* TOKEN     = "vt_b8Rt2Wq...";

bool sendReadings(float temp, float rh, int co2) {
  char body[96];
  snprintf(body, sizeof body, "{\"temp\":%.1f,\"rh\":%.0f,\"co2\":%d}", temp, rh, co2);

  WiFiClientSecure tls;
  tls.setCACert(ROOT_CA);                  // the server's root certificate, as a PEM string
  HTTPClient http;
  http.begin(tls, URL);
  http.addHeader("Authorization", String("Bearer ") + TOKEN);
  int code = http.POST(body);
  http.end();
  return code == 200;
}
```

One request adds a point to `env.temp`, `env.rh` and `env.co2`, all with the server's time. The body is a fixed-size `snprintf`, so there's no JSON library and no allocation.

If the board was offline, it can send buffered readings later with their own time, by adding `?t=1791449000000` to the URL. The board needs the real time for that, from NTP.

### The page

```html
<!doctype html>
<meta charset="utf-8">
<title>Room</title>
<style>
  body { font: 16px system-ui; margin: 2rem }
  .now { display: flex; gap: 2rem; font-size: 2rem }
  svg { width: 100%; height: 200px }
  path { fill: none; stroke: #36c; stroke-width: 2; vector-effect: non-scaling-stroke }
</style>

<div class="now">
  <span><span id="temp">?</span> °C</span>
  <span><span id="rh">?</span> %</span>
  <span><span id="co2">?</span> ppm</span>
</div>
<p>Updated <span id="at">never</span></p>
<svg viewBox="0 0 1000 200" preserveAspectRatio="none"><path /></svg>

<script type="module">
const API = "https://kv.example.com/v1/room-slate-hinge-cocoa-pilot/shared"

async function loadChart() {
  const points = []
  let since = Date.now() - 24 * 3600 * 1000
  while (since !== null) {
    const res = await fetch(`${API}/env.temp/series?since=${since}`)
    if (!res.ok) return
    const page = await res.json()
    points.push(...page.points)
    since = page.next
  }
  draw(points)
}

function draw(points) {
  if (points.length < 2) return
  const t0 = points[0].t, t1 = points.at(-1).t
  const values = points.map(p => p.value)
  const lo = Math.min(...values), hi = Math.max(...values)
  const x = t => (t - t0) / (t1 - t0) * 1000
  const y = v => 195 - (v - lo) / (hi - lo || 1) * 190
  const d = points.map((p, i) => (i ? "L" : "M") + x(p.t) + "," + y(p.value)).join("")
  document.querySelector("path").setAttribute("d", d)
}

async function watchNow() {
  let version = 0
  for (;;) {
    try {
      const res = await fetch(`${API}/env.*/watch?version=${version}`)
      if (res.status === 410) { version = 0; continue }
      if (!res.ok) throw new Error(res.status)
      const body = await res.json()
      for (const c of body.changes) {
        if (c.deleted) continue
        document.getElementById(c.key.slice("env.".length)).textContent = c.value
        document.getElementById("at").textContent = new Date(c.t).toLocaleTimeString()
      }
      version = body.version
    } catch {
      await new Promise(r => setTimeout(r, 5000))
    }
  }
}

loadChart()
setInterval(loadChart, 5 * 60 * 1000)
watchNow()
</script>
```

### Things this setup gets right

- **One request per reading cycle.** The pattern in `env.*/series` spreads the object over three series. Posting the same body to `env/series` would store one point holding an object, which is also valid, just not what the chart reads.
- **No token on the page.** Reading points, current values and watching are all plain `GET`s.
- **The live values and the chart come from the same keys.** Watching `env.*` gives the newest point of each series, with its time in `t`. The chart reads the full points from `env.temp/series`.
- **The chart pages through `next`.** A day at one point a minute is 1440 points, more than one response holds.
- **Old readings go by themselves.** Each series keeps its newest 50,000 points, about 35 days at this rate, so the board never needs a cleanup job and never starts failing.
- **The server doesn't average anything.** The page draws every point. For longer ranges it would thin them out itself.

### The same with curl

```
$ S=https://kv.example.com/v1/room-slate-hinge-cocoa-pilot/shared

$ curl -s -X POST -H "Authorization: Bearer $T" \
    --data-binary '{"temp":21.4,"rh":45,"co2":612}' "$S/env.*/series"
{"version":1791449600061,"t":1791449600042,"keys":["env.temp","env.rh","env.co2"]}

$ curl -s "$S/env.*"
{"items":[{"key":"env.co2","value":612,"version":1791449600061,"t":1791449600042}, ...],"version":1791449600061,"next":null}

$ curl -s "$S/env.temp/series?limit=2"
{"key":"env.temp","points":[{"t":1791449540017,"value":21.3},{"t":1791449600042,"value":21.4}],"version":1791449600061,"next":null}

$ cubby series room-slate-hinge-cocoa-pilot/shared/env.temp --since 1h
1791446040033	21.1
1791446100028	21.1
...
```

## 6. Family list (private space, invite links)

A shopping list for one household. Nobody outside it can read it, or even tell that it exists. The person who starts the list invites the others with links and can remove anyone later.

The space uses the default settings, so it's private. The creator owns it, and everyone else gets in by opening an invite link. The sharing UI is the ready-made `<cubby-share>` panel, so the page itself only deals with the list.

### The page

```html
<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>Family list</title>
<style>
  body { font: 16px system-ui, sans-serif; max-width: 30rem; margin: 2rem auto; padding: 0 1rem; }
  li.done span { text-decoration: line-through; color: #888; }
  #error { color: #b00020; }
</style>
<script type="module" src="https://kv.example.com/share.js"></script>

<h1>Family list <cubby-share id="share" members></cubby-share></h1>
<form id="add"><input id="text" placeholder="Add an item" required> <button>Add</button></form>
<ul id="items"></ul>
<p id="error"></p>

<script type="module">
import { kv, createSpace } from "https://kv.example.com/kv.js"

const $ = selector => document.querySelector(selector)
const showError = err => { $("#error").textContent = err.message }

// The list is the space name in the fragment. No list yet: start one. Private is the default.
let list = new URLSearchParams(location.hash.slice(1)).get("list")
let ownerToken = null
if (!list) {
  ({ space: list, ownerToken } = await createSpace({ label: "family" }))
  history.replaceState(null, "", `#list=${list}`)
}

const db = kv(list)
const me = await db.open()      // redeems #t=... if this was opened from an invite

if (!me.inside) {
  document.body.textContent = me.inviteFailed
    ? "This invite has expired or was already used. Ask for a new link."
    : "This list doesn't exist, or you haven't been invited. Ask for an invite link."
  throw new Error("not in this list")
}

// The share panel shows a Share button to owners, and "shared with you" to members.
// Right after creating the list, it opens once at the recovery key step.
$("#share").setAttribute("space", list)
if (ownerToken) $("#share").ownerToken = ownerToken

// ---------- the list ----------

const items = new Map()   // key -> { text, done }

db.shared.watch("item.*", change => {
  if (change.deleted) items.delete(change.key)
  else items.set(change.key, change.value)
}, { onSync: render, onError: showError })

$("#add").addEventListener("submit", async event => {
  event.preventDefault()
  const text = $("#text").value.trim()
  const key = `item.${String(Date.now()).padStart(13, "0")}.${Math.random().toString(36).slice(2, 6)}`
  try {
    await db.shared.set(key, { text, done: false })
    $("#text").value = ""
  } catch (err) { showError(err) }
})

function render() {
  $("#items").replaceChildren(...[...items].sort(([a], [b]) => a.localeCompare(b)).map(([key, item]) => {
    const li = document.createElement("li")
    li.className = item.done ? "done" : ""
    const box = document.createElement("input")
    box.type = "checkbox"
    box.checked = item.done
    box.onchange = () => db.shared.update(key, cur => cur && { ...cur, done: box.checked }).catch(showError)
    const span = document.createElement("span")
    span.textContent = ` ${item.text} `
    const del = document.createElement("button")
    del.textContent = "×"
    del.onclick = () => db.shared.delete(key).catch(showError)
    li.append(box, span, del)
    return li
  }))
}
</script>
```

### Without the panel

A page with its own UI uses `db.share()` and `qrSvg()` instead. Everything the panel does is a few calls:

```js
import { qrSvg } from "https://kv.example.com/share.js"

if (me.owner) {
  const { link } = await db.share({ label: "Grandma", uses: 1 })   // member, one use, 7 days
  linkInput.value = link                                            // this page's URL with &t=at_... added
  qrBox.innerHTML = qrSvg(link)

  for (const g of await db.owner.grants.list()) { /* show g.source's label, a Remove button: */ }
  await db.owner.grants.revokeUser(someone)
}
```

### Notes

- **Private needed no code.** `createSpace({ label: "family" })` is private because that's the default. A stranger with the URL gets `no_space`, the same as for a name nobody created.
- **`db.open()` is the whole entry step.** It redeems the invite in the URL, sending this browser's visitor token if it has one, so someone who already uses the list on this browser keeps one identity. It never throws for "not invited". It returns `inside: false`, and the page says so in plain words that cover both possible causes.
- **One person, one invite.** The panel's first choice is a one-use link per person. A link forwarded by mistake is worthless once the intended person has opened it, and the member list shows names instead of "group link".
- **Removing someone is one click** in the panel, `revokeUser` underneath. Their next request gets `no_space`, and their open watch ends on its next answer.
- **The recovery key is shown once, right after creating.** Clearing the browser loses the owner's identity. Redeeming the owner token with `join`, from any device, makes that device an owner again. The panel can rotate it later.
- **The token rides on reads as `?token=`,** so every read and watch in this private space works without a CORS preflight.

## Other situations

Short sketches, without full code, of how other pages would use settings, grants and the three areas.

| Page | `public` and grants | `shared/` | `users/{id}/` | `me/` |
|---|---|---|---|---|
| Kanban board for a small team | `none`, each member invited | `card.{id}` per card, so moving one card doesn't conflict with editing another | `name` | `collapsed` columns |
| Party playlist | `all` | `queue.{time}` per song anyone adds | `name` | |
| Sign-up sheet for time slots | `respond`, organiser owns it | `slots`, the available times | `slot`, the time each person picked, so nobody can remove someone else | |
| Turn-based game between friends | `none`, created per game by `createSpace`, the opponent gets a one-use invite | `board` and `turn`, written with `ifVersion` so two moves can't both land | `name` | |
| Status page | `read`, the script holds `shared/status:w` | `status` | | |
| Event schedule with RSVPs | `respond` | `schedule` from the organiser | `rsvp` per visitor | `reminders` |
| Quiz | `respond` | `questions`, and `answers` once the author reveals them | `answer.{n}` per question | `progress` |
| Leaderboard someone could cheat on | `respond` | | `score`, reported by each player, read with `users/*/score` | |
| Leaderboard that can't be faked | `read`, a trusted script holds `shared/scores:w` | `scores`, written only after the script checks results | | |
| Plant watering log | `none`, the sensor holds `shared/moisture:w`, the household are members | series `moisture` from the sensor, series `watered` with a note per watering | | |
| Personal habit tracker | `none`, one owner on several devices through the owner token | | | series `weight`, `sleep` |
| Team dashboard on a wall screen | `none`, editors as members, the screen with a one-use `viewer` invite | `widget.{id}` | | |

Patterns that come up repeatedly:

- **Many people adding things:** a key per item in each visitor's `users/` area, read together with `users/*/`. No conflicts, and authorship comes free.
- **One object several people edit:** one key in `shared/`, always written with `ifVersion` or `update()`.
- **Many independent items everyone edits:** a key per item in `shared/`, like `card.{id}`, so edits to different items never conflict.
- **People who know each other:** the default private space, an invite link per person from `db.share()` or the share panel, `revokeUser` to remove someone.
- **Author content plus visitor responses:** `public: respond`, the author as owner, responses in `users/`.
- **Machines that write:** one access token per machine with a narrow scope and `uses: 1`, redeemed once into a visitor token the machine keeps. `public: read` if anyone should see the results.
- **Readings over time:** a series key per measurement, added to with `POST {key}/series`, or several at once with `{prefix}*/series`. Read the current value like any key, and the past with `{key}/series`.
