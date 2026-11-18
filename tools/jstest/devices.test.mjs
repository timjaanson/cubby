// visitor(): pairing codes, accept, devices, sign out.
const S = process.env.S
const { kv, visitor, KvError } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }
const opt = (k, extra = {}) => ({ server: S, keyName: k, ...extra })

// The phone owns a private space.
const phone = visitor(opt("phone"))
const { space } = await phone.createSpace({ prefix: "notes" })
const phoneId = (await phone.id())
assert(/^u_/.test(phoneId), "phone id " + phoneId)

const link = await phone.pair({ base: "https://example.com/page/?q=1#space=" + space + "&invite=inv_x" })
assert(/^[0-9A-Z]{10}$/.test(link.code) && link.display === link.code.slice(0, 5) + "-" + link.code.slice(5), "code " + JSON.stringify(link))
const u = new URL(link.url)
const h = new URLSearchParams(u.hash.slice(1))
assert(h.get("pair") === link.code && h.get("space") === space && !h.has("invite") && u.search === "?q=1", "url " + link.url)
const done = link.done

// The laptop already has an identity and a live connection in another space.
const { space: other } = await visitor(opt("laptop")).createSpace({ prefix: "scratch", anyone: "all" })
const scratch = kv(other, opt("laptop", { live: true }))
await scratch.shared.set("x", 1)
const laptop = visitor(opt("laptop"))
const before = (await laptop.id())
assert(before && before !== phoneId, "laptop has its own id")
assert(await laptop.accept() === null, "nothing to accept")

const r = await laptop.accept(link.display.toLowerCase())
assert(r.id === phoneId && r.previous === before && r.deleted === true, "accept " + JSON.stringify(r))
assert((await laptop.id()) === phoneId, "laptop is the phone's visitor now")
const { device } = await done
assert(device.id === r.device && device.label === "Node.js", "done " + JSON.stringify(device))

// Same visitor in the private space, and the live connection followed.
const notes = kv(space, opt("laptop"))
await notes.shared.set("todo", "milk")
assert((await notes.shared.get("todo")).by === phoneId, "by after link")
await scratch.shared.set("y", 2)
const y = await scratch.shared.get("y")
assert(y.by === phoneId, "live write after link " + JSON.stringify(y))

// Used once.
const again = await visitor(opt("tablet")).accept(link.code).catch(e => e)
assert(again instanceof KvError && again.code === "pairing_code_used", "second use " + again?.code)

// List and rename.
let list = await laptop.devices.list()
assert(list.length === 2 && list[1].current && list[1].pairedBy === list[0].id, "list " + JSON.stringify(list))
list = await laptop.devices.rename("Work laptop")
assert(list[1].label === "Work laptop", "rename")

// A cancelled link rejects done.
const l2 = await laptop.pair({ ttl: 60 })
const p2 = l2.done.catch(e => e)
assert(await l2.cancel() === "cancelled", "cancel state")
const e2 = await p2
assert(e2 instanceof KvError && e2.code === "pairing_code_cancelled", "cancelled done " + e2?.code)

// Sign out the phone: its token stops working, and it can't see the space.
const removed = await laptop.devices.removeOthers()
assert(removed.length === 1 && removed[0] === list[0].id, "removed " + JSON.stringify(removed))
const phoneDb = kv(space, opt("phone"))
const e3 = await phoneDb.shared.get("todo").catch(e => e)
assert(e3 instanceof KvError && e3.code === "no_space", "phone after sign out " + e3?.code)
const e4 = await laptop.devices.remove("current").catch(e => e)
assert(e4 instanceof KvError && e4.code === "last_device", "last device " + e4?.code)

// The phone was signed out: its key comes back as a new visitor, alone.
const fresh = await phone.devices.list()
assert(fresh.length === 1 && fresh[0].current && (await phone.id()) && (await phone.id()) !== phoneId, "phone after sign out " + JSON.stringify(fresh) + " " + (await phone.id()))

scratch.close?.()
console.log("devices ok")
process.exit(0)
