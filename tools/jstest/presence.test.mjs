// Last wills and presence through kv.js, over live and HTTP.
const S = process.env.S
const { kv, createSpace } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const sleep = ms => new Promise(r => setTimeout(r, ms))
const until = async (f, m, ms = 4000) => {
  const end = Date.now() + ms
  while (Date.now() < end) { if (await f()) return; await sleep(10) }
  assert(false, "timeout: " + m)
}
const sockets = []
class TrackedWS extends WebSocket { constructor(u) { super(u); sockets.push(this) } }

const { space } = await createSpace({ server: S, tokenKey: "w-owner", public: "all" })
const watcher = kv(space, { server: S, tokenKey: "w-watch", live: true })
const heard = []
watcher.users.onPulse("here", p => heard.push(p))
await sleep(200)

// A will fires when the socket dies, and is registered again after a reconnect.
{
  const a = kv(space, { server: S, tokenKey: "w-a", live: true, WebSocket: TrackedWS })
  await a.shared.get("x")
  await until(() => a.live.status === "open", "socket")
  const clear = await a.user().will("here", { left: true })
  assert(typeof clear === "function", "will resolves a clear function")
  sockets.at(-1).close() // the server sees the connection end
  await until(() => heard.some(p => p.value?.left && p.user), "will fired")
  a.shared.get("x").catch(() => {}) // the next use opens a socket again
  await until(() => a.live.status === "open", "reconnect", 6000)
  await sleep(200) // re-registered on open
  heard.length = 0
  sockets.at(-1).close()
  await until(() => heard.some(p => p.value?.left), "will fired again after reconnect", 6000)
  a.shared.get("x").catch(() => {})
  await until(() => a.live.status === "open", "reconnect again", 6000)
  await sleep(200)
  clear()
  await sleep(200)
  heard.length = 0
  sockets.at(-1).close()
  await sleep(500)
  assert(!heard.some(p => p.value?.left), "cleared will doesn't fire")
  // Errors surface at registration.
  const other = await kv(space, { server: S, tokenKey: "w-b" }).id()
  const err = await a.user(other).will("here", 1).catch(e => e)
  assert(err?.code === "not_yours", "will into another's area " + err?.code)
  // Over HTTP it's a no-op.
  const h = kv(space, { server: S, tokenKey: "w-http" })
  assert(typeof await h.user().will("here", { left: true }) === "function", "http will is a no-op")
}

// Presence: join, value changes, a closed tab leaves through its will.
{
  const mk = (name, extra = {}) => kv(space, { server: S, tokenKey: "p-" + name, live: true, ...extra })
  const a = mk("a", { WebSocket: TrackedWS }), b = mk("b")
  const pa = a.users.presence("room", { value: { name: "A" } })
  const pb = b.users.presence("room", { value: { name: "B" } })
  const joins = [], leaves = []
  pb.onJoin = (id, v) => joins.push(v.name)
  pb.onLeave = id => leaves.push(id)
  await until(() => pb.peers.size === 1 && pa.peers.size === 1, "both see each other")
  const aId = await a.id()
  assert(pb.peers.get(aId).value.name === "A" && joins.join() === "A", "value and onJoin " + joins)
  let changes = 0
  pb.onChange = () => changes++
  pa.set({ name: "A2" })
  await until(() => pb.peers.get(aId)?.value.name === "A2" && changes > 0, "value change")
  // A's tab dies: its socket ends without stop(). Leaves via the will, well before the 6 s timeout.
  const t0 = Date.now()
  a.live.onStatus = null
  sockets.at(-1).close()
  await until(() => leaves.includes(aId), "left through will", 3000)
  console.log(`presence: will leave in ${Date.now() - t0} ms`)
  pa.stop(); pb.stop()
}

// Presence over HTTP: the same code; stop() leaves at once, silence by timeout.
{
  let cut = false
  const cuttable = (url, o) => cut && o?.method === "POST" ? Promise.reject(new Error("offline")) : fetch(url, o)
  const a = kv(space, { server: S, tokenKey: "h-a", fetch: cuttable }), b = kv(space, { server: S, tokenKey: "h-b" })
  const pa = a.users.presence("hall", { value: 1, interval: 200, onError: () => {} })
  const pb = b.users.presence("hall", { value: 2, interval: 200 })
  await until(() => pb.peers.size === 1 && pa.peers.size === 1, "http presence")
  const aId = await a.id()
  cut = true // a goes quiet without saying goodbye
  await until(() => !pb.peers.has(aId), "timeout leave", 3000)
  cut = false
  await until(() => pb.peers.has(aId), "back")
  pa.stop()
  await until(() => !pb.peers.has(aId), "stop leaves at once", 1000)
  pb.stop()
}
console.log("presence kv.js OK")
process.exit(0)
