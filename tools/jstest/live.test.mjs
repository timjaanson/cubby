// kv.js with { live: true } against a real server: the same answers as HTTP,
// requests and watches over the socket, recovery after a drop, fallback.
const S = process.env.S
const { kv, createSpace, KvError } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const sleep = ms => new Promise(r => setTimeout(r, ms))
const until = async (f, m, ms = 3000) => {
  const end = Date.now() + ms
  while (Date.now() < end) { if (await f()) return; await sleep(10) }
  assert(false, "timeout: " + m)
}

// Counts HTTP data requests and keeps hold of sockets, to check what went where.
let dataFetches = 0
const countingFetch = (url, o) => {
  if (/\/v1\/[^/]+\/(shared|me|users)\//.test(String(url))) dataFetches++
  return fetch(url, o)
}
const sockets = []
class TrackedWS extends WebSocket {
  constructor(url) { super(url); sockets.push(this) }
}

const { space } = await createSpace({ server: S, tokenKey: "owner" })
const http = kv(space, { server: S, tokenKey: "owner" })
const db = kv(space, { server: S, tokenKey: "owner", live: true, fetch: countingFetch, WebSocket: TrackedWS })
assert(http.live.status === "http" && db.live.status === "closed", "initial status " + db.live.status)
const seen = []
db.live.onStatus = s => seen.push(s)

// The first request goes over HTTP and starts the socket.
await db.shared.set("warm", 1)
await until(() => db.live.status === "open", "socket opens")
assert(seen.join() === "connecting,open", "status changes " + seen)
const before = dataFetches

// Same answers as HTTP.
{
  const v = await db.shared.set("note", { text: "milk", n: 1.5 }, { ttl: 30, ifVersion: 0 })
  const a = await db.shared.getEntry("note"), b = await http.shared.getEntry("note")
  assert(a.version === v && JSON.stringify(a) === JSON.stringify(b), "getEntry " + JSON.stringify([a, b]))
  assert(a.expires - a.t === 30000 && a.mine, "metadata")
  assert(db.writeBudget.visitor.remaining >= 0 && db.writeBudget.observedAt > 0, "write budget")
  assert(await db.shared.get("missing") === null, "missing key")
  await db.shared.set("t", "text")
  assert(await db.shared.get("t") === "text", "string value")
  const l1 = await db.shared.list("*"), l2 = await http.shared.list("*")
  assert(JSON.stringify(l1.items) === JSON.stringify(l2.items), "list")
  await db.shared.add("temp", 21.5)
  await db.shared.add("env.*", { rh: 40, co2: 600 })
  const pts = await db.shared.series("temp")
  assert(pts.points.length === 1 && pts.points[0].value === 21.5, "series")
  let err = await db.shared.set("note", 2, { ifVersion: 1 }).catch(e => e)
  assert(err instanceof KvError && err.code === "version_conflict" && err.status === 412, "conflict " + err.code)
  err = await db.shared.set("late", 1, { ifBefore: 0 }).catch(e => e)
  assert(err.code === "too_late" && err.details.t > 0, "deadline")
  assert(await db.shared.delete("t") === true && await db.shared.delete("t") === false, "delete")
  assert(await db.shared.update("count", n => (n || 0) + 1) === 1, "update")
  await db.me.set("pref", "dark")
  await db.user().set("name", "Tim")
  assert(await db.me.get("pref") === "dark" && await http.me.get("pref") === "dark", "me area")
  assert(await db.user().get("name") === "Tim" && (await db.users.list("name")).items.length === 1, "users area")
}
assert(dataFetches === before, `data requests used HTTP while open: ${dataFetches - before}`)

// A stranger gets the same no_space over the socket.
{
  const stranger = kv(space, { server: S, tokenKey: "stranger", live: true })
  await stranger.shared.get("note").catch(() => {}) // over HTTP, opens the socket
  await until(() => stranger.live.status === "open", "stranger socket")
  const err = await stranger.shared.get("note").catch(e => e)
  assert(err.code === "no_space" && err.status === 404, "stranger " + err.code)
}

// Watch: snapshot, HTTP writes pushed, socket writes seen over HTTP.
{
  const got = []
  let syncs = 0
  const stop = db.shared.watch("item.*", c => got.push(c), { onSync: () => syncs++ })
  await until(() => syncs === 1, "initial sync")
  const t0 = Date.now()
  await http.shared.set("item.1", "a")
  await until(() => got.some(c => c.key === "item.1"), "http write pushed")
  const latency = Date.now() - t0
  // HTTP watchers see socket writes.
  const httpGot = []
  const stopHttp = http.shared.watch("item.*", c => httpGot.push(c), { from: "now" })
  await sleep(100)
  await db.shared.set("item.2", "b")
  await until(() => httpGot.some(c => c.key === "item.2"), "socket write reaches http watch")
  stopHttp()
  await until(() => got.some(c => c.key === "item.2" && c.mine), "own write pushed")
  console.log(`live push latency ${latency} ms`)

  // The socket drops: a write in between still arrives, once.
  const mark = seen.length
  sockets.at(-1).close()
  await until(() => seen.slice(mark).includes("closed"), "drop noticed")
  await http.shared.set("item.3", "c")
  await until(() => got.some(c => c.key === "item.3"), "write during the drop arrives", 5000)
  assert(got.filter(c => c.key === "item.3").length === 1, "no duplicates")
  assert(db.live.status === "open", "reconnected " + db.live.status)
  stop()
  await sleep(50)
  const n = got.length
  await http.shared.set("item.4", "d")
  await sleep(200)
  assert(got.length === n, "no pushes after stop")
}

// A "now" watch syncs at once over the socket.
{
  let synced = false
  const stop = db.shared.watch("x.*", () => {}, { from: "now", onSync: () => { synced = true } })
  await until(() => synced, "now watch syncs", 1000)
  stop()
}

// Anonymous reads, then the first write creates a visitor and authenticates
// the same socket.
{
  const { space: pub } = await createSpace({ server: S, tokenKey: "owner", public: "all" })
  const guest = kv(pub, { server: S, tokenKey: "guest-" + Date.now(), live: true })
  const got = []
  const stop = guest.shared.watch("*", c => got.push(c))
  await until(() => guest.live.status === "open", "guest socket")
  await guest.shared.set("hello", "world")
  await until(() => got.some(c => c.key === "hello" && c.mine), "guest write over socket")
  assert((await guest.id()).startsWith("u_"), "guest visitor")
  stop()
}

// A stored token the server doesn't know: renewed, then the write goes through.
{
  const mem = new Map()
  globalThis.localStorage = { getItem: k => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, v), removeItem: k => mem.delete(k) }
  const { space: pub } = await createSpace({ server: S, tokenKey: "owner", public: "all" })
  mem.set("stale", JSON.stringify({ token: "vt_" + "x".repeat(43), id: "u_aaaaaaaaaaaaaaaa" }))
  const d = kv(pub, { server: S, tokenKey: "stale", live: true })
  await d.shared.get("a")
  await until(() => d.live.status === "open", "stale socket")
  await d.shared.set("a", 1)
  assert(JSON.parse(mem.get("stale")).token !== "vt_" + "x".repeat(43), "token renewed over socket")
  delete globalThis.localStorage
}

// No WebSocket possible: falls back to HTTP for good, everything still works.
{
  class Broken { constructor() { setTimeout(() => this.onclose?.(), 5) } close() {} }
  const d = kv(space, { server: S, tokenKey: "owner", live: true, WebSocket: Broken })
  await d.shared.get("note")
  await sleep(50)
  await d.shared.get("note")
  await until(() => d.live.status === "http", "fallback")
  let synced = false
  const stop = d.shared.watch("item.*", () => {}, { onSync: () => { synced = true } })
  await until(() => synced, "watch over HTTP after fallback")
  stop()
}

// A socket that went dead without a close event (a phone that slept):
// becoming visible pings it, and silence drops it within seconds. Requests
// meanwhile go over HTTP.
{
  const doc = new EventTarget()
  doc.hidden = false
  globalThis.document = doc
  let opened = 0
  class Silent { constructor() { this.readyState = 0; opened++; setTimeout(() => { this.readyState = 1; this.onopen?.() }, 5) } send() {} close() { this.readyState = 3 } }
  const d = kv(space, { server: S, tokenKey: "owner", live: true, WebSocket: Silent })
  const states = []
  d.live.onStatus = st => states.push(st)
  await d.shared.get("note")
  await until(() => d.live.status === "open", "silent socket opens")
  doc.hidden = true; doc.dispatchEvent(new Event("visibilitychange"))
  doc.hidden = false; doc.dispatchEvent(new Event("visibilitychange"))
  await until(() => states.includes("closed"), "dead socket dropped", 4500)
  delete globalThis.document
}

console.log("live kv.js OK")
process.exit(0)
