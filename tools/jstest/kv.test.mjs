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

console.log("kv.js OK")
