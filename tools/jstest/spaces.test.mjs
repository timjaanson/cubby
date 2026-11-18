// visitor().spaces() and db.batch(): listing your spaces, all-or-nothing writes.
const S = process.env.S
const { kv, visitor, KvError } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const opt = (k, extra = {}) => ({ server: S, keyName: k, ...extra })
const sleep = ms => new Promise(r => setTimeout(r, ms))

// No key yet: an empty list, and nothing registered.
let l = await visitor(opt("fresh")).spaces()
assert(l.items.length === 0 && l.next === null, "fresh " + JSON.stringify(l))
assert((await kv("trip-cedar-pilot-mango-frost", opt("fresh")).open()).id === null, "spaces() does not register")

const ann = visitor(opt("ann"))
const { space: a } = await ann.createSpace({ prefix: "groceries" })
await sleep(3)
const { space: b } = await ann.createSpace({ prefix: "trip" })
l = await ann.spaces()
assert(l.items.map(i => i.space).join() === [b, a].join() && l.items[0].access === "owner" && l.items[0].via === "created", "ann " + JSON.stringify(l))
const p1 = await ann.spaces({ limit: 1 })
const p2 = await ann.spaces({ limit: 1, after: p1.next })
assert(p1.items[0].space === b && p2.items[0].space === a && p2.next === null, "paging")
const bad = await ann.spaces({ colour: 1 }).catch(e => e)
assert(bad instanceof KvError && bad.code === "invalid_option", "unknown option")

// Ben joins by invite; his second device lists the same.
const { invite } = await kv(a, opt("ann")).invites.create({ access: "post", base: "https://example.com/" })
await kv(a, opt("ben")).open({ invite })
const code = await visitor(opt("ben")).pair()
await visitor(opt("ben-phone")).accept(code.code)
l = await visitor(opt("ben-phone")).spaces()
assert(l.items.length === 1 && l.items[0].space === a && l.items[0].access === "post" && l.items[0].members === 2, "ben's phone " + JSON.stringify(l))

// batch: several keys, consecutive versions, one push to a watcher.
for (const live of [false, true]) {
  const db = kv(b, opt("ann", { live }))
  const seen = []
  const w = db.shared.watch("*", c => seen.push(c))
  await sleep(300)
  await db.shared.set("old" + live, 1)
  const r = await db.batch([
    { set: "shared/title" + live, value: "Trip" },
    { set: "by/me/cursor" + live, value: { x: 1 }, ttl: 60 },
    { delete: "shared/old" + live },
  ])
  assert(r.items.length === 3 && r.items[1].version === r.items[0].version + 1 && r.version === r.items[2].version && r.items[0].created, "batch " + JSON.stringify(r))
  assert((await db.shared.get("title" + live)).value === "Trip" && await db.shared.get("old" + live) === null, "batch landed")
  assert((await db.by("me").get("cursor" + live)).expires > Date.now(), "ttl in batch")

  // One write fails: nothing lands, the error says which.
  const err = await db.batch([
    { set: "shared/n" + live, value: 1 },
    { set: "shared/title" + live, value: "x", ifVersion: 1 },
  ]).catch(e => e)
  assert(err instanceof KvError && err.code === "version_conflict" && err.details.index === 1, "conflict " + err?.code + " " + JSON.stringify(err?.details))
  assert(await db.shared.get("n" + live) === null, "rolled back")
  // Series points: own times, a ttl, the same series twice.
  const t0 = Date.now() - 60000
  const p = await db.batch([
    { set: "shared/now" + live, value: { temp: 21.4 } },
    { add: "shared/m1" + live, value: { temp: 21.3 }, t: t0, ttl: 3600 },
    { add: "shared/m1" + live, value: { temp: 21.4 }, t: t0 + 60000, ttl: 3600 },
  ])
  assert(p.items[1].t === t0 && p.items[2].t === t0 + 60000 && p.items[2].version === p.items[1].version + 1, "points in batch " + JSON.stringify(p))
  const pts = await db.shared.series("m1" + live, { order: "asc" })
  assert(pts.items.length === 2 && pts.items[0].expires === t0 + 3600000, "point ttl " + JSON.stringify(pts))
  const newest = await db.shared.series("m1" + live, { limit: 1 })
  assert(newest.items[0].t === t0 + 60000 && newest.next === t0 + 60000, "newest first " + JSON.stringify(newest))
  const older = await db.shared.series("m1" + live, { after: newest.next })
  assert(older.items.length === 1 && older.items[0].t === t0 && older.next === null, "after " + JSON.stringify(older))
  const window = await db.shared.series("m1" + live, { since: t0, before: new Date(t0 + 60000) })
  assert(window.items.length === 1 && window.items[0].t === t0, "since and before " + JSON.stringify(window))
  // Several series at once, merged by time, each point with its key.
  const both = await db.shared.series("m*")
  assert(both.items.filter(i => i.key === "m1" + live).length === 2 && both.items.every((i, n) => i.key && (!n || i.t <= both.items[n - 1].t)), "pattern " + JSON.stringify(both))
  // A check: the batch only lands if the key is still at that version.
  const nowV = (await db.shared.get("now" + live)).version
  const ok = await db.batch([{ check: "shared/now" + live, ifVersion: nowV }, { set: "shared/after" + live, value: 1 }])
  assert(ok.items[0].version === nowV, "check passes " + JSON.stringify(ok))
  const stale = await db.batch([{ check: "shared/now" + live, ifVersion: nowV - 1 }, { set: "shared/after" + live, value: 2 }]).catch(e => e)
  assert(stale.code === "version_conflict" && stale.details.index === 0 && (await db.shared.get("after" + live)).value === 1, "check fails " + stale.code)
  await db.shared.add("m5" + live, 1, { ttl: "1m" })
  assert((await db.shared.series("m5" + live)).items[0].expires > Date.now(), "add with ttl")
  const tOnSet = await db.batch([{ set: "shared/a", value: 1, t: 5 }]).catch(e => e)
  assert(tOnSet.code === "bad_batch", "t only on points")
  const shape = await db.batch([{ set: "shared/a", value: 1, colour: 2 }]).catch(e => e)
  assert(shape.code === "bad_batch" && shape.details.index === 0, "local shape check")
  await sleep(300)
  w.stop?.()
  assert(seen.some(c => c.key === "title" + live) && seen.some(c => c.key === "old" + live && c.deleted), "watch saw batch " + live)
}
// A reader can't batch-write shared/.
const { invite: ro } = await kv(b, opt("ann")).invites.create({ access: "read", base: "https://example.com/" })
const cat = kv(b, opt("cat"))
await cat.open({ invite: ro })
const denied = await cat.batch([{ set: "shared/x", value: 1 }]).catch(e => e)
assert(denied.code === "no_access" && denied.details.index === 0, "reader " + denied.code)
// An owner invite: one use, and the visitor who redeems it is an owner.
const own = await kv(b, opt("ann")).invites.create({ access: "owner", base: "https://example.com/" })
assert(own.access === "owner" && own.uses === 1 && own.expires > Date.now(), "owner invite " + JSON.stringify(own))
const dan = kv(b, opt("dan"))
assert((await dan.open({ invite: own.invite })).access === "owner", "owner join")
const tooMany = await kv(b, opt("ann")).invites.create({ access: "owner", uses: 2 }).catch(e => e)
assert(tooMany.code === "invalid_parameter", "owner uses " + tooMany.code)
console.log("spaces + batch ok")
process.exit(0)
