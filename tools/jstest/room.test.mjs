// openRoom: create once, find it again, members share their own links.
const S = process.env.S
const { openRoom } = await import(process.env.MOD)
const assert = (c, m) => { if (!c) { console.error("FAIL:", m); process.exit(1) } }

const a = await openRoom("board", { server: S, tokenKey: "room-a" })
assert(a.created && a.me.inside && a.me.owner && a.space.startsWith("board-"), "created " + JSON.stringify({ created: a.created, space: a.space }))
const again = await openRoom("board", { server: S, tokenKey: "room-a" })
assert(!again.created && again.space === a.space, "remembered under the label")

const link = await a.link({ base: "https://page.test/board/#s=" + a.space })
assert(link === await a.link({ base: "https://page.test/board/#s=" + a.space }), "same link while it works")
const t = new URL(link).hash.match(/t=(at_[\w-]+)/)[1]
assert(new URL(link).hash.startsWith("#s=" + a.space), "link keeps the room " + link)

// Someone opens the link on another device, and shares on.
const b = await openRoom("board", { server: S, tokenKey: "room-b", space: a.space, invite: t })
assert(b.me.inside && b.me.joined && !b.me.owner && b.me.canInvite && !b.created, "b joins")
const bLink = await b.link({ base: "https://page.test/board/#s=" + a.space })
assert(bLink !== link, "b mints its own link")
const c = await openRoom("board", { server: S, tokenKey: "room-c", space: a.space, invite: new URL(bLink).hash.match(/t=(at_[\w-]+)/)[1] })
assert(c.me.inside, "c joins through b")

// A revoked link is replaced, not handed out again.
const tokens = await a.db.tokens.list()
await a.db.tokens.update(tokens.find(x => x.by === a.me.id).id, { uses: 0 })
const fresh = await a.link({ base: "https://page.test/board/#s=" + a.space })
assert(fresh !== link, "used-up link replaced")

// A room that's gone: inside is false; forget clears it, create: false makes none.
await a.db.owner.deleteSpace()
const gone = await openRoom("board", { server: S, tokenKey: "room-a" })
assert(!gone.me.inside && !gone.created, "gone room")
gone.forget()
const none = await openRoom("board", { server: S, tokenKey: "room-a", create: false })
assert(none.db === null && !none.me.inside, "create false")
console.log("room kv.js OK")
process.exit(0)
