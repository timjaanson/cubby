const S = process.env.S
const { kv, createSpace, inviteLink, KvError } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const opt = k => ({ server: S, tokenKey: k })

// Owner creates a private space without a stored identity.
const { space, ownerToken } = await createSpace({ label: "family", ...opt("owner") })
assert(space.startsWith("family-") && ownerToken.startsWith("ot_"), "createSpace")
const owner = kv(space, opt("owner"))
let me = await owner.open({ invite: null })
assert(me.inside && me.owner && me.settings.public === "none", "owner open " + JSON.stringify(me))
await owner.shared.set("item.1", { text: "milk" })
assert((await owner.shared.get("item.1")).text === "milk", "owner read")

// A stranger sees nothing, and open() doesn't throw.
const stranger = kv(space, opt("stranger"))
me = await stranger.open({ invite: null })
assert(!me.inside && !me.can("read", "shared"), "stranger outside")
let err = await stranger.shared.get("item.1").catch(e => e)
assert(err instanceof KvError && err.code === "no_space", "stranger get " + err?.code)

// Share, join through open().
const inv = await owner.share({ label: "Anna", uses: 1, base: "https://example.com/list/#list=" + space })
assert(inv.link.startsWith("https://example.com/list/#list=" + space + "&t=at_") && inv.uses === 1, "share " + inv.link)
const anna = kv(space, opt("anna"))
me = await anna.open({ invite: inv.token })
assert(me.inside && me.joined && !me.owner && me.can("write", "shared", "item.2"), "anna open " + JSON.stringify(me))
await anna.shared.set("item.2", { text: "bread" })
const { items } = await anna.shared.list("item.*")
assert(items.length === 2, "anna list")

// Watch in a private space: snapshot arrives.
await new Promise((res, rej) => {
  const stop = anna.shared.watch("item.*", () => {}, {
    onSync: ({ initial }) => { if (initial) { stop(); res() } },
    onError: e => rej(e),
  })
})

// The used-up invite fails cleanly.
me = await stranger.open({ invite: inv.token })
assert(!me.inside && me.inviteFailed, "used invite")

// Members can't do owner things.
err = await anna.owner.tokens.list().catch(e => e)
assert(err.code === "owner_only", "member owner call " + err.code)

// Owner lists and removes Anna.
const grants = await owner.owner.grants.list()
const g = grants.find(g => !g.owner)
assert(g && g.source === inv.id, "grants list")
await owner.owner.grants.revokeUser(g.user)
err = await anna.shared.get("item.1").catch(e => e)
assert(err.code === "no_space", "removed member")

// Settings: open it up for reading.
const st = await owner.owner.settings({ public: "read" })
assert(st.public === "read", "settings")
assert((await stranger.shared.get("item.1")).text === "milk", "public read")
err = await stranger.shared.set("x", 1).catch(e => e)
assert(err.code === "no_access", "public write " + err.code)

// Owner token on a second device.
const laptop = kv(space, opt("laptop"))
me = await laptop.open({ invite: ownerToken })
assert(me.owner, "owner token join")
const fresh = await laptop.owner.rotateOwnerToken()
assert(fresh.startsWith("ot_") && fresh !== ownerToken, "rotate")

// Unknown option is refused locally.
err = await owner.share({ use: 1 }).catch(e => e)
assert(err.code === "invalid_option", "typo option")

// Fixed visitor token option.
const fixed = kv(space, { server: S, token: (await (await fetch(S + "/v1/anon", { method: "POST" })).json()).token })
assert((await fixed.id()).startsWith("u_"), "fixed id")
assert((await fixed.shared.get("item.1")).text === "milk", "fixed read")

// A stale visitor token in storage (unknown to the server) doesn't block
// createSpace or join: the client drops it and the server makes a new visitor.
{
  const mem = new Map()
  globalThis.localStorage = { getItem: k => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, v), removeItem: k => mem.delete(k) }
  const key = "cubby:" + S
  mem.set(key, JSON.stringify({ token: "vt_" + "x".repeat(43), id: "u_aaaaaaaaaaaaaaaa" }))
  const { space: s2 } = await createSpace({ server: S })
  assert(JSON.parse(mem.get(key)).token !== "vt_" + "x".repeat(43), "stale token replaced")
  const d2 = kv(s2, { server: S })
  assert((await d2.open({ invite: null })).owner, "owner after stale token")
  const t2 = await d2.share({ base: "https://example.com/" })
  mem.set(key, JSON.stringify({ token: "vt_" + "y".repeat(43), id: "u_bbbbbbbbbbbbbbbb" }))
  assert((await d2.open({ invite: t2.token })).joined, "join after stale token")
  mem.set(key, JSON.stringify({ token: "kv_" + "z".repeat(43), id: "u_cccccccccccccccc" }))
  await createSpace({ server: S })
  assert(JSON.parse(mem.get(key)).token.startsWith("vt_"), "old kv_ token dropped")
  delete globalThis.localStorage
}



// TTL, write metadata, limits, and deadlines through the public client API.
{
  const key = "metadata-owner"
  const { space: s } = await createSpace({ ...opt(key) })
  const db = kv(s, opt(key))
  const { limits, usage } = await db.limits()
  assert(limits.writesPerMinute === 120 && limits.writeBurst === 30 && usage.keys === 0, "client limits")
  const v = await db.shared.set("lease", "holder", { ttl: 5, ifVersion: 0 })
  const e = await db.shared.getEntry("lease")
  assert(e.version === v && e.t > 0 && e.expires - e.t === 5000, "client expiry metadata")
  assert(db.writeBudget.visitor.remaining === 29 && db.writeBudget.ip.remaining >= 0, "client write budget")
  await db.shared.set("lease", "holder", { ttl: 10, ifVersion: v })
  const renewed = await db.shared.getEntry("lease")
  assert(renewed.version === v && renewed.t === e.t && renewed.expires > e.expires, "silent heartbeat")
  await db.shared.set("lease", "holder")
  assert(!(await db.shared.getEntry("lease")).expires, "clear TTL")
  for (const write of [() => db.shared.set("late", 1, { ifBefore: 0 }), () => db.shared.delete("lease", { ifBefore: 0 }), () => db.shared.add("points", 1, { ifBefore: 0 })]) {
    const err = await write().catch(e => e)
    assert(err.code === "too_late" && err.details.t > 0 && !err.retryable, "deadline forwarded")
  }
  const timing = await db.clock.sync({ samples: 2 })
  assert(timing.rtt >= 0 && Math.abs(db.clock.now() - Date.now()) < 1000, "real server clock")
  const fired = await new Promise(resolve => db.clock.at(Math.ceil(db.clock.now()) + 20, () => resolve(db.clock.now())))
  assert(fired > 0, "clock timer")
  let canceled = false
  db.clock.at(Math.ceil(db.clock.now()) + 20, () => { canceled = true })()
  await new Promise(r => setTimeout(r, 30))
  assert(!canceled, "clock cancellation")
  db.clock.dispose()
  // This exercises the real binary's one-second sweep, not a manual sweep.
  await db.user().set("online", true, { ttl: 5 })
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => { stop(); reject(new Error("TTL sweep did not notify watch")) }, 8000)
    const stop = db.users.watch("online", change => {
      if (change.expired && change.deleted) { clearTimeout(timeout); stop(); resolve() }
    }, { onError: err => { clearTimeout(timeout); stop(); reject(err) } })
  })
}

// A 429 is exposed once, without silently retrying the write.
{
  let requests = 0
  const db = kv(space, { server: S, token: "vt_fixed", fetch: async url => {
    if (String(url).includes("/whoami")) return new Response(JSON.stringify({ id: "u_aaaaaaaaaaaaaaaa" }))
    requests++
    return new Response(JSON.stringify({ error: "rate_limited", message: "Wait", retryAfter: 7 }), { status: 429, headers: { RateLimit: '\"visitor\";r=0;t=15' } })
  } })
  const err = await db.shared.set("x", 1).catch(e => e)
  assert(requests === 1 && err.retryAfter === 7 && err.details.retryAfter === 7 && err.retryable, "top-level retryAfter")
  assert(db.writeBudget.visitor.remaining === 0, "429 budget")
}

// Prefer the shortest clock sample and re-arm existing timers after a sync.
{
  let sample = 0
  const db = kv(space, { server: S, fetch: async () => {
    const i = sample++
    await new Promise(r => setTimeout(r, i === 0 ? 30 : 1))
    return new Response(JSON.stringify({ t: Date.now() + (i === 0 ? 1000 : 5000) }))
  } })
  const timing = await db.clock.sync({ samples: 3 })
  assert(timing.offset > 4900 && timing.offset < 5100, "fastest clock sample")
  db.clock.dispose()
}

// Browser offline events abort a pending watch, report offline immediately,
// and pause requests until online. Simulate the browser event APIs in Node.
{
  const savedNavigator = Object.getOwnPropertyDescriptor(globalThis, "navigator")
  const events = new EventTarget()
  globalThis.window = events
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: { onLine: true } })
  let reads = 0, synced, offline, pendingSignal
  const firstSync = new Promise(r => { synced = r })
  const offlineError = new Promise(r => { offline = r })
  const db = kv(space, { ...opt("offline-test"), fetch: (...args) => {
    if (String(args[0]).includes("/watch")) { reads++; pendingSignal = args[1].signal }
    return fetch(...args)
  } })
  const stop = db.shared.watch("item.*", () => {}, { onSync: synced, onError: offline })
  await firstSync
  await new Promise(r => setTimeout(r, 30))
  navigator.onLine = false
  events.dispatchEvent(new Event("offline"))
  const err = await Promise.race([offlineError, new Promise((_, reject) => setTimeout(() => reject(new Error("offline event timeout")), 1000))])
  const count = reads
  assert(err.code === "network_error" && pendingSignal.aborted, "offline abort")
  await new Promise(r => setTimeout(r, 30))
  assert(reads === count, "no requests while offline")
  navigator.onLine = true; events.dispatchEvent(new Event("online"))
  await new Promise(r => setTimeout(r, 30))
  assert(reads > count, "online resumes watch")
  stop(); await new Promise(r => setTimeout(r, 10)); delete globalThis.window
  if (savedNavigator) Object.defineProperty(globalThis, "navigator", savedNavigator)
  else delete globalThis.navigator
}
console.log("kv.js OK")
