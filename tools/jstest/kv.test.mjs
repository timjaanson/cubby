const S = process.env.S
const { kv, visitor, KvError } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const opt = k => ({ server: S, keyName: k })

// Owner creates a private space without a stored identity.
const { space } = await visitor(opt("owner")).createSpace({ prefix: "family" })
assert(space.startsWith("family-"), "createSpace")
const owner = kv(space, opt("owner"))
let me = await owner.open({ invite: null })
assert(me.access === "owner" && me.space === space && me.settings.anyone === "none" && me.settings.inviters === "post" && me.expires === null, "owner open " + JSON.stringify(me))
const written = await owner.shared.set("item.1", { text: "milk" })
let entry = await owner.shared.get("item.1")
assert(entry.value.text === "milk" && entry.version === written.version && entry.key === "item.1" && entry.mine, "owner read " + JSON.stringify(entry))
assert(await owner.shared.get("nope") === null, "missing is null")

// A stranger sees nothing, and open() doesn't throw.
const stranger = kv(space, opt("stranger"))
me = await stranger.open({ invite: null })
assert(me.access === "none" && !me.can("read") && me.invite === null, "stranger outside")
let err = await stranger.shared.get("item.1").catch(e => e)
assert(err instanceof KvError && err.code === "no_space", "stranger get " + err?.code)

// Invite, join through open(). The link carries the space and the invite.
const inv = await owner.invites.create({ label: "Anna", uses: 1, base: "https://example.com/list/#tab=2" })
assert(inv.link === `https://example.com/list/#tab=2&space=${space}&invite=${inv.invite}` && inv.uses === 1 && inv.expires > Date.now(), "invite " + JSON.stringify(inv))
const defaults = await owner.invites.create({ base: "https://example.com/" })
assert(defaults.uses === null && defaults.access === "all" && defaults.expires - Date.now() > 6.9 * 864e5, "invite defaults " + JSON.stringify(defaults))
const anna = kv(space, opt("anna"))
me = await anna.open({ invite: inv.invite })
assert(me.access === "all" && me.invite === "redeemed" && me.can("write", "shared") && me.can("write", "by"), "anna open " + JSON.stringify(me))
await anna.shared.set("item.2", { text: "bread" })
let page = await anna.shared.list("item.*")
assert(page.items.length === 2 && page.next === null && page.version > 0, "anna list")
page = await anna.shared.list("item.*", { limit: 1, order: "desc" })
assert(page.items.length === 1 && page.items[0].key === "item.2" && page.next, "list page " + JSON.stringify(page))
page = await anna.shared.list("item.*", { limit: 1, order: "desc", after: page.next })
assert(page.items[0].key === "item.1" && page.next === null, "list page 2")

// Watch in a private space: snapshot arrives.
await new Promise((res, rej) => {
  const stop = anna.shared.watch("item.*", () => {}, {
    onSync: ({ initial }) => { if (initial) { stop(); res() } },
    onError: e => rej(e),
  })
})

// The used-up invite fails cleanly.
me = await stranger.open({ invite: inv.invite })
assert(!me.can("read") && me.invite === "invalid", "used invite")

// Members can't do owner things, but they can invite, within their own level.
err = await anna.settings({ anyone: "read" }).catch(e => e)
assert(err.code === "owner_only", "member owner call " + err.code)
assert((await anna.open()).canInvite === true && me.canInvite === false, "member canInvite, stranger not")
const fromAnna = await anna.invites.create({ label: "ben", uses: 1, base: "https://page.test/board" })
assert(fromAnna.link.includes("#space=" + space + "&invite=inv_") && fromAnna.access === "all", "member invite " + JSON.stringify(fromAnna))
assert((await anna.invites.list()).length === 1, "member sees own invites")
const ben = kv(space, opt("ben"))
me = await ben.open({ invite: fromAnna.invite })
assert(me.can("read") && me.invite === "redeemed", "ben joins through anna")

// Owner lists members, with the invite chain, and removes Anna.
const members = await owner.members.list()
const annaId = (await anna.open()).id, benId = (await ben.open()).id
const a = members.find(m => m.id === annaId), b = members.find(m => m.id === benId)
assert(a?.access === "all" && a.label === "Anna" && b?.invitedBy === annaId && "lastSeen" in a, "members with chain " + JSON.stringify(members))
assert((await ben.members.list()).every(m => !("invitedBy" in m)), "members see less")
await owner.members.set(benId, "post")
err = await ben.shared.set("x", 1).catch(e => e)
assert(err.code === "no_access" && err.details.need === "all", "post can't write shared " + err.code)
await ben.by("me").set("vote", 1)
assert(await owner.members.remove(annaId) === true, "remove anna")
err = await anna.shared.get("item.1").catch(e => e)
assert(err.code === "no_space", "removed member")
assert((await ben.shared.get("item.1")).value.text === "milk", "ben stays after anna is removed")

// A level with an end shows when it ends.
await owner.members.set(benId, "post", { ttl: "1h" })
me = await ben.open()
assert(me.access === "post" && me.expires > Date.now() + 3500e3, "level expires " + JSON.stringify(me))

// Settings: open it up for reading.
const st = await owner.settings({ anyone: "read" })
assert(st.anyone === "read" && st.inviters === "post", "settings")
assert((await stranger.shared.get("item.1")).value.text === "milk", "anyone reads")
err = await stranger.shared.set("x", 1).catch(e => e)
assert(err.code === "no_access", "stranger write " + err.code)
assert((await stranger.open({ invite: null })).access === "read", "stranger level")

// Leaving makes you a stranger again.
assert(await ben.members.remove("me") === true && (await ben.open()).access === "read", "leave")

// "*" areas only read, and say so.
err = await owner.by("*").set("x", 1).catch(e => e)
assert(err.code === "read_only", "by(*) is read-only " + err.code)
assert((await owner.by("*").list("vote")).items.some(i => i.by === benId), "by(*) lists every author")

// A backup key brings the owner back on a new device; the backup stays.
const backup = await visitor(opt("owner")).backup()
assert(JSON.parse(backup).key.d && JSON.parse(backup).server === S, "backup " + backup)
const restored = await visitor(opt("laptop")).restore(backup)
const laptop = kv(space, opt("laptop"))
me = await laptop.open({ invite: null })
assert(me.access === "owner" && restored.id === me.id, "restored owner " + JSON.stringify(me))
assert((await visitor(opt("owner")).devices.list()).length === 3, "owner, backup and laptop are devices")

// Unknown options are refused locally, everywhere.
for (const call of [() => owner.invites.create({ use: 1 }), () => owner.shared.set("x", 1, { ttll: 5 }), () => kv(space, { apikey: "x" }), () => owner.shared.series("x", { until: 1 })]) {
  err = await Promise.resolve().then(call).catch(e => e)
  assert(err.code === "invalid_option", "typo option " + err.code)
}

// An API key: one space, at most its level.
const made = await visitor(opt("owner")).apiKeys.create(space, { label: "hook", access: "read" })
assert(made.key.startsWith("cubby_") && made.expires > Date.now(), "api key " + JSON.stringify(made))
const hook = kv(space, { server: S, apiKey: made.key })
assert((await hook.shared.get("item.1")).value.text === "milk", "api key read")
err = await hook.shared.set("y", 1).catch(e => e)
assert(err.code === "no_access", "api key level " + err.code)
assert((await visitor(opt("owner")).apiKeys.list()).some(k => k.label === "hook"), "api key listed")

// Credentials from a backup key, as a Node script uses them.
const script = kv(space, { server: S, credentials: JSON.parse(backup) })
assert(await visitor({ server: S, credentials: JSON.parse(backup) }).id() === restored.id && (await script.shared.get("item.1")).value.text === "milk", "credentials")

// A device that was signed out: its key comes back as a new visitor, which
// doesn't see the old space, and createSpace still works.
{
  const temp = visitor(opt("temp"))
  const { space: s2 } = await temp.createSpace()
  const before = await temp.id()
  const code = await temp.pair()
  const other = visitor(opt("temp2"))
  await other.accept(code.code)
  const gone = (await other.devices.list()).find(d => !d.current).id
  await other.devices.remove(gone)
  me = await kv(s2, opt("temp")).open({ invite: null })
  assert(me.access === "none", "signed-out device " + JSON.stringify(me))
  const { space: s3 } = await temp.createSpace()
  assert((await kv(s3, opt("temp")).open({ invite: null })).access === "owner", "new visitor creates spaces")
  assert((await temp.id()) !== before, "new visitor id")
}

// TTL, write metadata, limits, and deadlines through the public client API.
{
  const key = "metadata-owner"
  const { space: s } = await visitor(opt(key)).createSpace()
  const db = kv(s, opt(key))
  const { limits, usage } = await db.open()
  assert(limits.writesPerMinute === 120 && limits.writeBurst === 30 && usage.keys === 0, "client limits")
  const { version: v } = await db.shared.set("lease", "holder", { ttl: 5, ifVersion: 0 })
  const e = await db.shared.get("lease")
  assert(e.version === v && e.t > 0 && e.expires - e.t === 5000, "client expiry metadata")
  await db.shared.set("lease", "holder", { ttl: "10s", ifVersion: v })
  const renewed = await db.shared.get("lease")
  assert(renewed.version === v && renewed.t === e.t && renewed.expires > e.expires, "silent heartbeat")
  await db.shared.set("lease", "holder")
  assert(!(await db.shared.get("lease")).expires, "clear TTL")
  for (const write of [() => db.shared.set("late", 1, { ifBefore: 0 }), () => db.shared.delete("lease", { ifBefore: new Date(0) }), () => db.shared.add("points", 1, { ifBefore: 0 })]) {
    const err = await write().catch(e => e)
    assert(err.code === "too_late" && err.details.t > 0 && !err.retryable, "deadline forwarded")
  }
  assert(await db.shared.delete("never-was") === null, "delete missing is null")
  const updated = await db.shared.update("count", n => (n ?? 0) + 1)
  assert(updated.value === 1 && updated.version > 0, "update returns the entry " + JSON.stringify(updated))
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
  await db.by("me").set("online", true, { ttl: 5 })
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => { stop(); reject(new Error("TTL sweep did not notify watch")) }, 8000)
    const stop = db.by("*").watch("online", change => {
      if (change.expired && change.deleted) { clearTimeout(timeout); stop(); resolve() }
    }, { onError: err => { clearTimeout(timeout); stop(); reject(err) } })
  })
}

// A 429 is exposed once, without silently retrying the write.
{
  let requests = 0
  const db = kv(space, { server: S, apiKey: "cubby_fixed", fetch: async url => {
    requests++
    return new Response(JSON.stringify({ error: "rate_limited", message: "Wait", retryAfter: 7 }), { status: 429 })
  } })
  const err = await db.shared.set("x", 1).catch(e => e)
  assert(requests === 1 && err.retryAfter === 7 && err.details.retryAfter === 7 && err.retryable, "top-level retryAfter")
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
