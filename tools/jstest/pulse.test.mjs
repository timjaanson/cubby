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
  await Promise.all(Array.from({ length: 60 }, (_, i) => a.user().pulse("cursor", { x: i + 1 }, { maxRate: 0 })))
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
  let err = await a.user(other).pulse("cursor", 1).catch(e => e)
  assert(err?.code === "not_yours", "not yours " + err?.code)
  err = await a.shared.pulse("pulse", 1).catch(e => e)
  assert(err?.code === "invalid_key", "pulse is not a key")
}
// The client throttle: at most maxRate per key, newest value wins, one warning per window.
{
  const warns = []
  const warn = console.warn
  console.warn = (...a) => { const m = a.join(" "); if (m.startsWith("cubby: pulses")) warns.push(m) }
  const a = kv(space, { server: S, tokenKey: "p-throttle" })
  const b = kv(space, { server: S, tokenKey: "p-guest" })
  const got = []
  const stop = b.users.onPulse("tilt", p => got.push(p.value))
  await sleep(150)
  const results = []
  for (let i = 1; i <= 50; i++) { results.push(a.user().pulse("tilt", i)); await sleep(2) }
  const r = await Promise.all(results)
  assert(r[0] === true && r[49] === true, "first and last sent " + r[0] + " " + r[49])
  const sent = r.filter(Boolean).length
  // ~100 ms of sends at 20/s: the first, about two slots, and the last.
  assert(sent >= 2 && sent <= 5, "sent within the rate: " + sent)
  await until(() => got.includes(50), "newest value arrives")
  assert(got.length === sent && got.every((v, i) => i === 0 || v > got[i - 1]), "in order, only sent ones " + JSON.stringify(got))
  await until(() => warns.length === 1, "one warning")
  assert(/users\/u_\w+\/tilt \d+ dropped \(limit 20\/s\)/.test(warns[0]), "warning names key and limit: " + warns[0])
  // More throttling right away is collected, not printed.
  for (let i = 0; i < 30; i++) a.user().pulse("tilt", 100 + i)
  await sleep(300)
  assert(warns.length === 1, "no second warning within the window")
  // pulseRate on kv() and maxRate per call.
  const fast = kv(space, { server: S, tokenKey: "p-throttle2", pulseRate: 1000 })
  const rf = await Promise.all(Array.from({ length: 5 }, async (_, i) => { await sleep(i * 3); return fast.user().pulse("x", i) }))
  assert(rf.filter(Boolean).length >= 4, "pulseRate raises the limit " + rf)
  const r0 = await Promise.all(Array.from({ length: 10 }, (_, i) => a.user().pulse("raw", i, { maxRate: 0 })))
  assert(r0.every(Boolean), "maxRate 0 sends all")
  console.warn = warn
  stop()
}
console.log("pulse kv.js OK")
process.exit(0)
