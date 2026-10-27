// Pulses through kv.js: never stored, delivered over HTTP and live alike.
const S = process.env.S
const { kv, createSpace } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const sleep = ms => new Promise(r => setTimeout(r, ms))
const until = async (f, m, ms = 4000) => {
  const end = Date.now() + ms
  while (Date.now() < end) { if (await f()) return; await sleep(5) }
  assert(false, "timeout: " + m)
}

const { space } = await createSpace({ server: S, tokenKey: "p-owner", public: "all" })
for (const [sendLive, listenLive] of [[false, false], [true, true], [false, true], [true, false]]) {
  const tag = `send ${sendLive ? "live" : "http"}, listen ${listenLive ? "live" : "http"}`
  const a = kv(space, { server: S, tokenKey: "p-sender-" + tag, live: sendLive }) // own pulse budget per round
  const b = kv(space, { server: S, tokenKey: "p-guest", live: listenLive })
  await b.id()
  const got = [], mine = []
  let missed = 0
  const stop = b.users.onPulse("cursor", p => got.push(p), { onMissed: () => missed++ })
  const stopMine = a.users.onPulse("cursor", p => mine.push(p))
  if (listenLive) await until(() => b.live.status === "open", tag + ": listener socket")
  if (sendLive) { await a.shared.get("x"); await until(() => a.live.status === "open", tag + ": sender socket") }
  await sleep(150) // listeners in place

  const t0 = Date.now()
  await a.user().pulse("cursor", { x: 0 })
  await until(() => got.length === 1, tag + ": first pulse")
  const ms = Date.now() - t0
  const p = got[0]
  assert(p.key === "cursor" && p.user === await a.id() && p.by === p.user && !p.mine && p.value.x === 0 && p.t > 0, tag + " pulse " + JSON.stringify(p))
  await until(() => mine.some(q => q.mine), tag + ": sender hears its own, marked mine")

  // A burst arrives complete and in order, without waiting on each send.
  await Promise.all(Array.from({ length: 60 }, (_, i) => a.user().pulse("cursor", { x: i + 1 })))
  await until(() => got.length === 61, tag + ": burst of 60, got " + got.length)
  // Parallel HTTP requests may reach the server in any order; one socket keeps it.
  const xs = got.map(q => q.value.x)
  assert(missed === 0 && (sendLive ? xs.every((x, i) => x === i) : new Set(xs).size === 61), tag + ": all arrived" + (sendLive ? " in order" : "") + " missed=" + missed + " n=" + xs.length + " uniq=" + new Set(xs).size + " " + JSON.stringify(xs))
  console.log(`${tag}: first pulse ${ms} ms`)
  stop(); stopMine()
}

// Never stored; others' areas are theirs; text works.
{
  const a = kv(space, { server: S, tokenKey: "p-owner", live: true })
  await a.shared.pulse("ping", "hello")
  assert(await a.shared.get("ping") === null, "pulse not stored")
  const other = await kv(space, { server: S, tokenKey: "p-guest" }).id()
  let err; try { a.user(other).pulse("cursor", 1) } catch (e) { err = e }
  err = err || await a.user(other).pulse("cursor", 1).catch(e => e)
  assert(err?.code === "not_yours", "not yours " + err?.code)
  err = await a.shared.pulse("pulse", 1).catch(e => e)
  assert(err?.code === "invalid_key", "pulse is not a key")
}
console.log("pulse kv.js OK")
process.exit(0)
