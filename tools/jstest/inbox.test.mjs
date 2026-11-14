// to/, inbox/ and bans through kv.js, over HTTP and live.
const S = process.env.S
const { kv, createSpace, KvError } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const opt = (k, extra = {}) => ({ server: S, tokenKey: k, ...extra })

const { space } = await createSpace({ label: "quiz", public: "respond", ...opt("host") })
for (const live of [false, true]) {
  const sfx = live ? "-live" : ""
  const host = kv(space, opt("host", { live }))
  const bob = kv(space, opt("bob" + sfx, { live }))
  const cara = kv(space, opt("cara" + sfx, { live }))
  await host.open({ invite: null })
  await bob.open({ invite: null })
  await cara.open({ invite: null })
  const hostId = await host.id(), bobId = await bob.id(), caraId = await cara.id()

  // Answers to the owners: one slot per sender.
  await bob.to("owners").set("answer", 1)
  await cara.to("owners").set("answer", 2)
  let { items } = await host.inbox.owners.list("answer")
  const answers = items.filter(i => i.from === bobId || i.from === caraId)
  assert(answers.length === 2 && answers.every(i => i.to === "owners"), "owners inbox " + JSON.stringify(items))
  assert(await host.inbox.owners.from(bobId).get("answer") === 1, "one sender")
  assert(await bob.to("owners").get("answer") === 1, "read back")
  const err = await bob.inbox.owners.list("*").catch(e => e)
  assert(err instanceof KvError && err.code === "owner_only", "member reads owners' inbox " + err?.code)

  // A dealt card, watched as it arrives.
  const got = new Promise((res, rej) => {
    const stop = bob.inbox.watch("hand", c => { if (!c.deleted) { stop(); res(c) } }, { from: "now", onError: rej })
  })
  await new Promise(r => setTimeout(r, 300))
  await host.to(bobId).set("hand", ["7H", "QS"])
  const c = await got
  assert(c.from === hostId && c.to === bobId && c.value[1] === "QS" && !c.mine, "dealt " + JSON.stringify(c))
  assert((await cara.inbox.list("hand")).items.length === 0, "cara can't see bob's hand")
  assert((await host.sent.list("hand")).items[0].to === bobId, "host's sent list")

  // A directed pulse.
  const pulse = new Promise(res => {
    const stop = host.inbox.onPulse("rtc.*", p => { stop(); res(p) })
  })
  await new Promise(r => setTimeout(r, 300))
  await bob.to(hostId).pulse("rtc.offer", { sdp: "x" })
  const p = await pulse
  assert(p.from === bobId && p.to === hostId && p.value.sdp === "x", "pulse " + JSON.stringify(p))

  // The recipient deletes.
  assert(await bob.inbox.from(hostId).delete("hand") === true, "recipient deletes")
  assert(await host.to(bobId).get("hand") === null, "gone for the sender too")
}

// Bans.
const host = kv(space, opt("host"))
const troll = kv(space, opt("troll"))
await troll.open({ invite: null })
await troll.user().set("name", "troll")
const trollId = await troll.id()
await host.owner.bans.add(trollId, { until: "1h" })
const me = await troll.open({ invite: null })
assert(!me.inside && me.banned && me.banned.until > Date.now(), "open while banned " + JSON.stringify(me))
const e = await troll.user().set("name", "x").catch(e => e)
assert(e.code === "banned" && e.status === 403 && !e.retryable, "write while banned " + e.code)
assert((await host.owner.bans.list()).some(b => b.user === trollId), "bans list")
assert(await host.owner.bans.remove(trollId) === true, "unban")
assert((await troll.open({ invite: null })).inside, "inside after unban")
console.log("inbox kv.js OK")
process.exit(0)
