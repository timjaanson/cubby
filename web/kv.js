// cubby client. A guide for whoever writes code with this, person or coding agent.
//
// Two handles:
//   kv(space)    one space: its data, its members, its invites
//   visitor()    you: your ID, your spaces, your devices, your API keys
//
// Spaces are PRIVATE by default: only the creator (the owner) and people they
// invite get in. Everyone else gets 404 no_space, exactly as if the space
// didn't exist, so a missing invite looks like a wrong name. Check both.
//
// Every page starts the same way:
//   const db = kv(SPACE)
//   const me = await db.open()     // redeems an invite link (#invite=...)
//   if (!me.can("read")) { /* show: "ask the owner for an invite link" */ }
//
// Everyone has one level per space. me.access is yours ("none" outside):
//   read    read the space, and your own inbox
//   post    also write your own keys: db.by("me"), db.to(id)
//   all     also write common keys: db.shared
//   owner   also moderate, change settings, manage members
//
// Areas mirror the HTTP paths:
//   db.shared             common keys, for "all": a title, a list, a board
//   db.by("me")           your own keys: only you write them, everyone reads them
//   db.by("*")            everyone's own keys together; each item's by is its author
//   db.to(id)             for one reader: a visitor ID, "owners", or "me" (yourself)
//   db.inbox("*")         what was sent to you; db.inbox.owners("*") for owners
// Messages, comments, votes and names go in db.by("me") and are read with
// db.by("*"), never in db.shared, so nobody can edit or delete someone
// else's. Owners can still delete them, as moderation.
//
// Every area has get, set, delete, update, add, series, list, watch, pulse,
// pulses and will. "*" areas only read: list, watch, pulses.
//
// Creating a space (once from a shell with `cubby new`, or in the page):
//   const { space } = await visitor().createSpace()   // this browser's visitor owns it
// To get back in from elsewhere: pair another device, or keep
// visitor().backup() somewhere safe. Your spaces on every device:
// visitor().spaces().
//
// Inviting. An invite gives a level, at most your own:
//   const { link } = await db.invites.create()                   // all, any uses, 7 days
//   const { link } = await db.invites.create({ access: "post" }) // can't change db.shared
// The link is this page with #space=NAME&invite=inv_... A page that takes its
// space from the link reads it with new URLSearchParams(location.hash.slice(1)).
//   Ready-made UI with link, QR code and members: import "<server>/share.js"
//   and put <cubby-share space="..."></cubby-share> in the page.
// Members: db.members.list(), .set(id, level, { ttl }) with "read", "post",
// "all", "owner" or "banned" (owners), .remove(id), .remove("me") to leave.
//
// Open a space to everyone only when the user asked for it:
//   visitor().createSpace({ anyone: "read" })  anyone reads: status pages
//   visitor().createSpace({ anyone: "post" })  anyone reads and posts their own: polls, RSVPs
//   visitor().createSpace({ anyone: "all" })   anyone with the name reads and writes
//
// Live interaction (a board, a quiz, a game)? kv(SPACE, { live: true }): the same
// API over one WebSocket, falling back to HTTP by itself.
// Pulses are values that are never stored, for cursors and reactions:
//   db.by("me").pulse("cursor", { x, y })
//   db.by("*").pulses("cursor", p => draw(p.by, p.value))
//   db.presence("here", { value: { name } })     // .peers, .onJoin, .onLeave
//
// Units: a time is ms since 1970 (or a Date); a duration (ttl, interval) is
// seconds or a string like "90s", "10m", "12h", "14d".
//
// Never put an invite (inv_...), API key (cubby_...) or backup key in page source:
// anyone who loads the page could use it. Invite links are for sending to people.
//
// Docs: docs/usage.md in the cubby repository. Exports: kv, visitor, KvError.

const SERVER = new URL(".", import.meta.url).href.replace(/\/$/, "")


export class KvError extends Error {
  constructor(status, code, message, details = {}) {
    super(message)
    this.name = "KvError"
    this.status = status
    this.code = code
    this.details = details
    this.retryAfter = details.retryAfter
  }
  // Worth trying again later: the network, the server or a rate limit failed,
  // not the request itself.
  get retryable() {
    return this.code === "network_error" || this.status === 429 || this.status >= 500
  }
}

const SPACE_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/
const KEY_RE = /^[A-Za-z0-9._:-]{1,128}$/
const PATTERN_RE = /^[A-Za-z0-9._:-]{0,127}\*$/
const VISITOR_RE = /^u_[a-z2-7]{16}$/

// "watch", "series" and "pulse" can follow a key in a path, so they aren't keys.
const validKey = k => typeof k === "string" && KEY_RE.test(k) && k !== "watch" && k !== "series" && k !== "pulse" && k !== "." && k !== ".."
const validTarget = t => validKey(t) || (typeof t === "string" && PATTERN_RE.test(t))

function checkVisitor(id) {
  if (typeof id === "string" && VISITOR_RE.test(id)) return
  throw new KvError(0, "invalid_key", `${JSON.stringify(id)} isn't a visitor ID. IDs look like u_k7m2x9q4p1r8t3w6.`)
}

// Checked before any request. Keys go into the URL, where "#", "?", "/" or
// ".." would silently address something else.
function checkKey(k, patternOk = false) {
  if (patternOk ? validTarget(k) : validKey(k)) return
  throw new KvError(0, "invalid_key",
    `${JSON.stringify(k)} isn't a valid ${patternOk ? "key or pattern" : "key"}. Keys are 1 to 128 of A-Z a-z 0-9 . _ : - and can't be ".", "..", "watch", "series" or "pulse".` +
    (patternOk ? " A pattern is a prefix followed by *." : ""))
}
// The 4096 words generated names are made of. The server fills this in when
// it serves the file, from the same list it uses itself.
const WORDS = "__CUBBY_WORDS__".split(" ")
const MAX_LABEL = 35

// A space name is 16 to 63 lowercase letters, digits and single hyphens.
const validSpace = name => typeof name === "string" && name.length >= 16 && name.length <= 63 && SPACE_RE.test(name)

// newName returns the label (cleaned up) and four random words, 48 random bits.
function newName(label = "") {
  label = String(label).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, MAX_LABEL).replace(/-+$/, "")
  // 65536 is a multiple of 4096, so the low 12 bits are unbiased. Four
  // 3-letter words would be one character short of a valid name; redraw those.
  let words
  do {
    words = Array.from(crypto.getRandomValues(new Uint16Array(4)), n => WORDS[n & 0xfff]).join("-")
  } while (words.length < 16)
  return label ? `${label}-${words}` : words
}

const sleep = ms => new Promise(r => setTimeout(r, ms))

// How long past the server's 25 s answer a watch request may take before the
// client treats the connection as dead. Phones drop connections while asleep
// without failing the request.
const WATCH_DEADLINE = 35000
const HIDDEN_PAUSE = 5 * 60 * 1000

// Live connection timing. Phones lose sockets silently when they sleep or
// switch networks, so the client pings and gives up on silence quickly.
const LIVE_OPEN_WAIT = 10000      // to connect
const LIVE_WATCH_WAIT = 3000      // a watch waits this long for a connecting socket, then uses HTTP
const LIVE_PING = 25000           // while open
const LIVE_PING_WAIT = 10000
const LIVE_WAKE_PING_WAIT = 3000  // when the page becomes visible again
const LIVE_REQUEST_WAIT = 30000

// Signing in. Each device has its own key pair: ES256 (ECDSA P-256), made
// on first use. The private key is a non-extractable CryptoKey in IndexedDB,
// so page scripts can use it while the page is open but nothing can copy it
// out. Requests carry a JWT the key signed, good for an hour, renewed five
// minutes before it ends and never stored. Pages don't see any of this.

const enc = new TextEncoder()
const b64u = buf => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")
const b64uJSON = o => b64u(enc.encode(JSON.stringify(o)))
const EC_KEY = { name: "ECDSA", namedCurve: "P-256" }
const TOKEN_TTL = 3600 // s
const TOKEN_RENEW = 300 // s before the end

// The public half as a JWK, members in RFC 7638's order, so its JSON is
// the canonical form the thumbprint (the device ID) hashes.
function publicJWK(j) {
  return j.kty === "OKP" ? { crv: j.crv, kty: j.kty, x: j.x } : { crv: j.crv, kty: j.kty, x: j.x, y: j.y }
}

async function makeSigner(privateKey, jwk) {
  jwk = publicJWK(jwk)
  const kid = b64u(await crypto.subtle.digest("SHA-256", enc.encode(JSON.stringify(jwk))))
  const alg = jwk.kty === "OKP" ? "EdDSA" : "ES256"
  return {
    jwk, kid, alg,
    // A token for the server at aud, issued at now (ms). withKey puts the
    // public key in the header, as registering and pairing need.
    async token(aud, now, withKey = false, ttl = TOKEN_TTL) {
      const header = { alg, typ: "JWT", kid }
      if (withKey) header.jwk = jwk
      const iat = Math.floor(now / 1000)
      const input = b64uJSON(header) + "." + b64uJSON({ aud, iat, exp: iat + ttl })
      const params = alg === "EdDSA" ? { name: "Ed25519" } : { name: "ECDSA", hash: "SHA-256" }
      return input + "." + b64u(await crypto.subtle.sign(params, privateKey, enc.encode(input)))
    },
  }
}

async function newKeyPair(extractable = false) {
  const pair = await crypto.subtle.generateKey(EC_KEY, extractable, ["sign", "verify"])
  return { privateKey: pair.privateKey, jwk: await crypto.subtle.exportKey("jwk", pair.publicKey), pair }
}

// A credentials object, as `cubby device backup` and devices().backup() make
// it: { server, visitor, device, key: private JWK }. The key is imported
// non-extractable.
async function importCredentials(c) {
  const key = typeof c === "string" ? JSON.parse(c).key : c?.key
  if (!key?.d) throw new KvError(0, "invalid_option", "credentials need key, a private JWK (with d).")
  const params = key.kty === "OKP" ? { name: "Ed25519" } : EC_KEY
  const privateKey = await crypto.subtle.importKey("jwk", key, params, false, ["sign"])
  return makeSigner(privateKey, key)
}

// IndexedDB, for keys; memory where there's none (Node, some private modes).
const keyStore = (() => {
  const mem = new Map()
  let dbp = null, warned = false
  function open() {
    if (!dbp) dbp = new Promise((resolve, reject) => {
      if (typeof indexedDB === "undefined") return reject(new Error("no IndexedDB"))
      const r = indexedDB.open("cubby", 1)
      r.onupgradeneeded = () => r.result.createObjectStore("keys")
      r.onsuccess = () => resolve(r.result)
      r.onerror = () => reject(r.error)
    })
    return dbp
  }
  async function op(mode, fn) {
    const db = await open()
    return new Promise((resolve, reject) => {
      const tx = db.transaction("keys", mode)
      const req = fn(tx.objectStore("keys"))
      tx.oncomplete = () => resolve(req.result)
      tx.onerror = tx.onabort = () => reject(tx.error)
    })
  }
  function fallback() {
    if (!warned) {
      warned = true
      console.warn("cubby: IndexedDB isn't available, so this device is new on every page load")
    }
  }
  return {
    async get(k) { try { return await op("readonly", s => s.get(k)) } catch { fallback(); return mem.get(k) } },
    async put(k, v) { mem.set(k, v); try { await op("readwrite", s => s.put(v, k)) } catch { fallback() } },
  }
})()

// One identity per server and key name, shared by every kv() instance on the
// page. get() signs in, registering a new key the first time; peek() only
// uses one that exists. Both answer { token, id }.
const identities = new Map()

function identity(server, keyName, fetchFn, { credentials, apiKey } = {}) {
  const aud = new URL(server).origin
  let skew = 0 // server clock minus ours, learned from a refused token
  async function learnSkew() {
    try {
      const t0 = Date.now()
      const res = await fetchFn(server + "/v1/time")
      const { t } = await res.json()
      if (t) skew = t - (t0 + Date.now()) / 2
    } catch {}
  }
  function http(res, body) {
    return new KvError(res.status, body.error || "http_error", body.message || res.statusText, body)
  }

  if (apiKey) {
    // A static API key: sent as is, for one space. Never stored.
    const v = { token: apiKey, id: null }
    return { fixed: true, peek: async () => v, get: async () => v, forget() {}, adopt() {}, learnSkew, signer: async () => null, visitorId: () => null, aud: () => aud, now: () => Date.now() + skew }
  }

  let signer = null, id = null, tok = null, tokEnd = 0, loading = null, pending = null
  const given = !!credentials
  async function load() {
    if (!loading) loading = (async () => {
      if (given) {
        signer = await importCredentials(credentials)
        id = (typeof credentials === "string" ? JSON.parse(credentials) : credentials).visitor || null
        return
      }
      const rec = await keyStore.get(keyName)
      if (rec?.privateKey && rec.jwk) {
        signer = await makeSigner(rec.privateKey, rec.jwk)
        id = rec.visitor || null
        ident.rec = rec
      }
    })()
    return loading
  }
  async function save() {
    if (!given) await keyStore.put(keyName, { privateKey: ident.rec.privateKey, jwk: ident.rec.jwk, visitor: id })
  }
  async function mint() {
    const now = Date.now() + skew
    if (!tok || now / 1000 > tokEnd - TOKEN_RENEW) {
      tok = await signer.token(aud, now)
      tokEnd = Math.floor(now / 1000) + TOKEN_TTL
    }
    return tok
  }
  const ident = {
    rec: null,
    fixed: false,
    given,
    learnSkew: async () => { await learnSkew(); tok = null },
    // The device's signer, made if there's none yet (not registered).
    async signer() {
      await load()
      if (!signer && !given) {
        const k = await newKeyPair()
        ident.rec = { privateKey: k.privateKey, jwk: k.jwk }
        signer = await makeSigner(k.privateKey, k.jwk)
        await save()
      }
      return signer
    },
    aud: () => aud,
    now: () => Date.now() + skew,
    // The visitor ID as known right now, without waiting: null before the
    // key is loaded or registered.
    visitorId: () => id,
    async peek() {
      await load()
      return signer && id ? { token: await mint(), id } : null
    },
    async get() {
      const cur = await ident.peek()
      if (cur) return cur
      if (given) {
        // Given credentials name their visitor, or say who they are.
        const res = await fetchFn(server + "/v1/me", { headers: { Authorization: "Bearer " + await mint() } })
        const body = await res.json().catch(() => ({}))
        if (!res.ok) throw http(res, body)
        id = body.id
        return { token: await mint(), id }
      }
      if (!pending) pending = (async () => {
        const s = await ident.signer()
        const res = await fetchFn(server + "/v1/visitors", {
          method: "POST", headers: { Authorization: "Bearer " + await s.token(aud, Date.now() + skew, true, 300) },
          body: JSON.stringify({ label: deviceLabel() }),
        })
        const body = await res.json().catch(() => ({}))
        if (!res.ok) throw http(res, body)
        id = body.id
        await save()
        return { token: await mint(), id }
      })().finally(() => { pending = null })
      return pending
    },
    // The server doesn't know this key any more (signed out, or a year
    // unused). The next get() registers it again, as a new visitor.
    forget() {
      if (given) return
      id = null
      tok = null
      if (ident.rec) save()
    },
    // This device joined another visitor, by pairing.
    async adopt(visitor) {
      id = visitor
      await save()
    },
  }
  if (given) return ident
  const k = server + "\n" + keyName
  if (identities.has(k)) return identities.get(k)
  identities.set(k, ident)
  return ident
}

// Marks kv.js's own pulses, whose throttling is expected and not worth a warning.
const QUIET = Symbol("quiet")

// Pulses dropped by the client throttle, or refused by the server with 429.
// One console.warn per 10 seconds at most, for all keys together: the first
// right away, then a summary of what happened in between.
const pulseReport = (() => {
  const WINDOW = 10000
  let counts = new Map(), last = -Infinity, timer = null
  function flush() {
    timer = null
    if (!counts.size) return
    const parts = []
    for (const [path, c] of counts) {
      const bits = []
      if (c.client) bits.push(`${c.client} dropped (limit ${c.rate}/s)`)
      if (c.server) bits.push(`${c.server} refused by the server (429)`)
      parts.push(`${path.replace(/\/pulse$/, "")} ${bits.join(", ")}`)
    }
    console.warn(`cubby: pulses throttled since the last report: ${parts.join("; ")}. ` +
      "The newest value per key is always sent. Pass { maxRate } to pulse() to change the limit.")
    counts = new Map(); last = Date.now()
  }
  return {
    count(path, kind, rate) {
      const c = counts.get(path) || { client: 0, server: 0, rate }
      c[kind]++
      if (rate) c.rate = rate
      counts.set(path, c)
      if (!timer) {
        timer = setTimeout(flush, Math.max(0, last + WINDOW - Date.now()))
        timer.unref?.() // don't keep Node alive for a log line
      }
    },
  }
})()


export function kv(space, opts = {}) {
  checkOptions(opts, ["server", "fetch", "keyName", "credentials", "apiKey", "WebSocket", "live"], "kv()")
  if (!validSpace(space)) {
    const suggestion = newName(String(space ?? ""))
    throw new KvError(0, "invalid_space", `"${space}" isn't a valid space name. Try ${suggestion}.`, { suggestion })
  }
  const server = (opts.server || SERVER).replace(/\/$/, "")
  const fetchFn = opts.fetch || globalThis.fetch.bind(globalThis)
  const ident = identity(server, opts.keyName || "cubby:" + server, fetchFn, { credentials: opts.credentials, apiKey: opts.apiKey })
  const WS = opts.WebSocket || globalThis.WebSocket
  const live = opts.live && typeof WS === "function" ? makeLive(server.replace(/^http/, "ws") + "/v1/" + space + "/live", WS) : null


  // auth: the request needs a signed-in device, and registers this one if
  // it isn't yet. Without auth, whatever token there is still goes along, so
  // a private space works with no option. Reads send it as ?token=: an
  // Authorization header makes the browser send a CORS preflight, cached per
  // URL, and every watch request has a new URL.
  async function request(method, path, { body, auth = false, query, signal } = {}) {
    const read = method === "GET" || method === "HEAD"
    for (let attempt = 0; ; attempt++) {
      const cur = auth ? await ident.get() : await ident.peek()
      let url = `${server}/v1/${space}${path ? "/" + path : ""}`
      const q = new URLSearchParams()
      for (const [k, v] of Object.entries(query || {})) if (v !== undefined && v !== null) q.set(k, String(v))
      if (read && cur?.token) q.set("token", cur.token)
      const qs = q.toString()
      if (qs) url += "?" + qs
      const headers = {}
      if (!read && cur?.token) headers.Authorization = "Bearer " + cur.token
      const dataPath = /^(shared|by|to|inbox)\//.test(path) || path === "batch"
      // With live on, data requests go over the socket while it's open and
      // over HTTP otherwise. The answers are the same either way.
      const viaLive = live && dataPath && live.isOpen()
      if (live && dataPath && !viaLive) live.ensure()
      let res
      try {
        if (viaLive) {
          const lq = {}
          for (const [k, v] of Object.entries(query || {})) if (v !== undefined && v !== null) lq[k] = String(v)
          res = await live.request(cur?.token, method, path, lq, body)
        } else {
          res = await fetchFn(url, { method, headers, body, signal })
        }
      } catch (err) {
        if (err?.name === "AbortError") throw err
        throw new KvError(0, "network_error", String(err?.message || err))
      }
      if (cur?.token && res.status === 401 && attempt === 0 && !ident.fixed) {
        const b = await res.clone().json().catch(() => ({}))
        if (b.error === "token_expired" || b.error === "token_not_yet_valid") {
          // This device's clock is off: sign again on the server's time.
          await ident.learnSkew()
          continue
        }
        if (b.error === "unknown_key" && !ident.given) {
          // The device was signed out, or unused for a year. Its key comes
          // back as a new visitor with no grants, so in a private space the
          // retry gets no_space.
          ident.forget()
          continue
        }
      }
      return res
    }
  }

  const fail = errorFrom

  async function sendPulse(path, body) {
    const res = await request("POST", path, { body, auth: true })
    if (res.ok) return true
    const err = await fail(res)
    if (err.status === 429) pulseReport.count(path, "server")
    throw err
  }

  // Per path: when the last send went out, and the value waiting for the next slot.
  const throttles = new Map()
  function throttledPulse(path, body, rate = 20, report = true) {
    if (!(rate > 0) || rate === Infinity) return sendPulse(path, body)
    const gap = 1000 / rate
    let t = throttles.get(path)
    if (!t) throttles.set(path, t = { last: -Infinity, waiting: null, timer: null })
    if (!t.waiting && Date.now() - t.last >= gap) {
      t.last = Date.now()
      return sendPulse(path, body)
    }
    if (t.waiting) {
      t.waiting.resolve(false)
      if (report) pulseReport.count(path, "client", rate)
    }
    return new Promise((resolve, reject) => {
      t.waiting = { body, resolve, reject }
      if (t.timer) return
      t.timer = setTimeout(() => {
        const w = t.waiting
        t.waiting = null; t.timer = null; t.last = Date.now()
        sendPulse(path, w.body).then(w.resolve, w.reject)
      }, Math.max(0, t.last + gap - Date.now()))
    })
  }

  // mine: this visitor made the last write or the delete.
  const withMine = e => ({ ...e, mine: !!e.by && e.by === ident.visitorId() })

  // An area is a path prefix like "shared" or "by/u_...". prefix may be a
  // function for "by/me", whose ID is known only after signing in. auth:
  // reads need a signed-in visitor. other: another visitor's area, where only
  // they write (owners may still delete; the server decides). byAuthor: the
  // area spans every author, so entries differ by who wrote them.
  function makeArea(prefix, { auth = false, other = null, byAuthor = false } = {}) {
    const pathOf = async target => `${typeof prefix === "function" ? await prefix() : prefix}/${target}`
    return { pathOf, auth, other, byAuthor }
  }

  function checkWrite(a) {
    if (a.other && ident.visitorId() !== a.other) throw new KvError(0, "not_yours", "This is another visitor's area. Only they can write it.")
  }

  function jsonBody(value) {
    const body = JSON.stringify(value)
    if (body === undefined) throw new KvError(0, "invalid_value", "That value can't be stored as JSON.")
    return body
  }

  // The methods of one area. Every write returns what the server answered.
  function areaMethods(a) {
    const m = {
      // The entry for key: { key, value, version, by, mine, t?, expires? },
      // the same shape as list items and watch changes. null if it doesn't exist.
      async get(key) {
        checkKey(key)
        const res = await request("GET", await a.pathOf(key), { auth: a.auth })
        if (await missingKey(res)) return null
        if (!res.ok) throw await errorFrom(res)
        const version = Number((res.headers.get("ETag") || "").replace(/"/g, ""))
        const text = await res.text()
        const isJSON = (res.headers.get("Content-Type") || "").startsWith("application/json")
        const entry = { key, value: isJSON ? JSON.parse(text) : text, version, by: res.headers.get("KV-By") || undefined }
        const t = res.headers.get("KV-Time")
        if (t) entry.t = Number(t)
        const expires = res.headers.get("KV-Expires")
        if (expires) entry.expires = Number(expires)
        return withMine(entry)
      },
      // { version }. Options: ifVersion (0: only if missing), ttl, ifBefore.
      async set(key, value, o = {}) {
        checkKey(key)
        checkOptions(o, ["ifVersion", "ttl", "ifBefore"], "set()")
        checkWrite(a)
        const res = await request("PUT", await a.pathOf(key), { body: jsonBody(value), auth: true, query: writeQuery(o) })
        if (!res.ok) throw await errorFrom(res)
        return res.json()
      },
      // { version }, or null if there was nothing to delete.
      async delete(key, o = {}) {
        checkKey(key)
        checkOptions(o, ["ifVersion", "ifBefore"], "delete()")
        const res = await request("DELETE", await a.pathOf(key), { auth: true, query: writeQuery(o) })
        if (await missingKey(res)) return null
        if (!res.ok) throw await errorFrom(res)
        return res.json()
      },
      // Read, change with fn, write back with ifVersion; again on a conflict.
      // fn gets the value (null if missing) and returns the new one, or
      // undefined to leave it. Resolves with the entry as it is now.
      async update(key, fn, o = {}) {
        checkOptions(o, ["ttl", "ifBefore", "retries"], "update()")
        const { retries = 10, ...write } = o
        for (let i = 0; ; i++) {
          const cur = await m.get(key)
          const next = fn(cur ? cur.value : null)
          if (next === undefined || (cur && JSON.stringify(next) === JSON.stringify(cur.value))) return cur
          try {
            const { version } = await m.set(key, next, { ...write, ifVersion: cur ? cur.version : 0 })
            return withMine({ key, value: next, version, by: ident.visitorId() })
          } catch (err) {
            if (err.code !== "version_conflict" || i >= retries) throw err
            await sleep(50 + Math.random() * 150)
          }
        }
      },
      // Adds a point to the series at key: { version, t }. Options: t (the
      // reading's own time; the same t replaces that point), ttl (how long
      // after t the point is kept), ifBefore.
      async add(key, value, o = {}) {
        checkKey(key)
        checkOptions(o, ["t", "ttl", "ifBefore"], "add()")
        checkWrite(a)
        const res = await request("POST", (await a.pathOf(key)) + "/series", { body: jsonBody(value), auth: true, query: writeQuery(o) })
        if (!res.ok) throw await errorFrom(res)
        return res.json()
      },
      // One page of points, newest first: { items: [{ t, value, expires? }],
      // version, next }. since (inclusive) and before (exclusive) bound them;
      // order "asc" gives oldest first; pass next back as after. A pattern
      // like "env.*" reads every series under it, merged by time, each item
      // with its key. null if a single key doesn't exist.
      async series(key, o = {}) {
        checkKey(key, true)
        checkOptions(o, ["since", "before", "order", "limit", "after"], "series()")
        const query = { since: timeMs(o.since), before: timeMs(o.before), order: o.order, limit: o.limit, after: o.after }
        const res = await request("GET", (await a.pathOf(key)) + "/series", { auth: a.auth, query })
        if (await missingKey(res)) return null
        if (!res.ok) throw await errorFrom(res)
        return res.json()
      },
      list: (pattern, o) => list(a, pattern, o),
      watch: (target, onChange, o) => watch(a, target, onChange, o),
      // Pulses are never stored. pulse() hands a value to whoever is
      // listening right now; nothing keeps it, so get() and watch() never
      // see it. For cursors, drags, reactions: things sent often, or that
      // matter as events rather than as state.
      //
      // At most maxRate per second per key (default 20). Faster sends wait
      // for the next slot, and a newer value replaces one still waiting, so
      // the latest always goes out. Resolves true when sent, false when
      // replaced. maxRate: 0 sends everything at once.
      async pulse(key, value, o = {}) {
        checkKey(key)
        checkOptions(o, ["maxRate", QUIET], "pulse()")
        checkWrite(a)
        const path = (await a.pathOf(key)) + "/pulse"
        return throttledPulse(path, jsonBody(value), o.maxRate ?? 20, !o[QUIET])
      },
      pulses: (target, onPulse, o) => listenPulses(a, target, onPulse, o),
      // A last will: a pulse the server sends for you when your live
      // connection ends without you clearing it, such as a closed tab. Only
      // with live: true; over HTTP it's kept and does nothing, so the same
      // code works either way. Resolves with a function that clears it.
      async will(key, value) {
        checkKey(key)
        checkWrite(a)
        const body = jsonBody(value)
        const path = (await a.pathOf(key)) + "/pulse"
        if (!live) return () => {}
        const res = await live.setWill((await ident.get()).token, path, body)
        if (res && !res.ok) throw await errorFrom(res)
        return () => live.clearWill(path)
      },
    }
    return m
  }

  // A "*" area spans several: it lists, watches and listens, and says so
  // when asked to do anything else.
  function readOnly(a, name, hint) {
    const all = areaMethods(a)
    const out = { list: all.list, watch: all.watch, pulses: all.pulses }
    for (const k of ["get", "set", "delete", "update", "add", "series", "pulse", "will"]) {
      out[k] = async () => { throw new KvError(0, "read_only", `${name} spans several areas: it lists, watches and listens to pulses. ${hint}`) }
    }
    return out
  }

  // One page of entries matching pattern, in key order (order "desc" for
  // the reverse): { items, version, next }. Pass next back as after for
  // the following page; version starts a watch right after this list.
  async function list(a, pattern, o = {}) {
    checkKey(pattern, true)
    checkOptions(o, ["limit", "order", "after"], "list()")
    const res = await request("GET", await a.pathOf(pattern), { auth: a.auth, query: { limit: o.limit, order: o.order, after: o.after } })
    if (!res.ok) throw await errorFrom(res)
    const body = await res.json()
    return { ...body, items: body.items.map(withMine) }
  }

  // Who else is here, from pulses on by/{id}/{key}: a heartbeat every
  // interval with your value, a leave pulse on stop() and pagehide, and a
  // last will with the same leave on a live connection, so a closed tab
  // leaves at once. A peer silent for timeout counts as gone, which is how
  // leaving shows over HTTP. peers never includes you.
  function presence(key, o = {}) {
    checkKey(key)
    checkOptions(o, ["value", "interval", "timeout", "onError"], "presence()")
    const interval = durationMs(o.interval ?? 2, "interval")
    const timeout = o.timeout === undefined ? interval * 3 : durationMs(o.timeout, "timeout")
    const warn = o.onError || (err => console.warn("cubby presence:", err))
    const hasWin = typeof window !== "undefined" && typeof window.addEventListener === "function"
    const mine = db.by("me")
    let value = o.value ?? null, stopped = false, clearWill = null
    const peers = new Map()
    const h = {
      peers,
      onJoin: null, onLeave: null, onChange: null,
      get value() { return value },
      set(v) { value = v; beat() },
      stop,
    }
    const call = (fn, ...args) => { try { fn?.(...args) } catch (err) { warn(err) } }
    const changed = () => call(h.onChange, peers)
    const leave = id => {
      if (!peers.delete(id)) return
      call(h.onLeave, id)
      changed()
    }
    // The helper's own sends coalesce on purpose, so they aren't reported.
    const beat = () => { if (!stopped) mine.pulse(key, { v: value }, { [QUIET]: true }).catch(warn) }
    const stopListen = db.by("*").pulses(key, p => {
      if (p.mine || !p.by || stopped) return
      const v = p.value
      if (v && typeof v === "object" && v.left === true) return leave(p.by)
      if (!v || typeof v !== "object" || !("v" in v)) return // not a presence pulse
      const now = Date.now(), cur = peers.get(p.by)
      if (!cur) {
        peers.set(p.by, { value: v.v, since: now, seen: now })
        call(h.onJoin, p.by, v.v)
        changed()
        beat() // so the newcomer sees us now, not on our next heartbeat
      } else {
        cur.seen = now
        if (JSON.stringify(cur.value) !== JSON.stringify(v.v)) { cur.value = v.v; changed() }
      }
    }, { onError: o.onError })
    const beatTimer = setInterval(beat, interval)
    const sweepTimer = setInterval(() => {
      const now = Date.now()
      for (const [id, p] of peers) if (now - p.seen > timeout) leave(id)
    }, Math.min(1000, interval / 2))
    const goodbye = () => mine.pulse(key, { left: true }, { maxRate: 0 }).catch(() => {})
    if (hasWin) window.addEventListener("pagehide", goodbye)
    mine.will(key, { left: true }).then(c => { if (stopped) c(); else clearWill = c }, warn)
    beat()
    function stop() {
      if (stopped) return
      stopped = true
      clearInterval(beatTimer)
      clearInterval(sweepTimer)
      stopListen()
      clearWill?.()
      if (hasWin) window.removeEventListener("pagehide", goodbye)
      goodbye()
      peers.clear()
    }
    return h
  }


  // after:
  //   0 (default)   every existing key first, then changes
  //   "now"         only changes from now on
  //   a version     continue after that version (a list's or watch's version)
  // After a 410 (the version is too old, say after a week asleep) a watch with
  // state starts again from 0 and reports keys it knew that are gone as
  // deleted. A "now" watch skips ahead to now instead. Either way the next
  // onSync has resync: true.
  function watch(a, target, onChange, o = {}) {
    const { pathOf, auth, byAuthor } = a
    checkKey(target, true)
    checkOptions(o, ["after", "onSync", "onError"], "watch()")
    let from = o.after ?? 0
    if (from !== "now" && !(Number.isSafeInteger(from) && from >= 0)) {
      throw new KvError(0, "invalid_option", "after takes 0, \"now\" or a version.")
    }
    const onlyNew = from === "now"
    // One entry: its key and, across several areas, whose it is. Under
    // shared/ by is just the last writer, so it isn't part of the entry.
    const entryId = e => JSON.stringify([byAuthor ? e.by || "" : "", e.from || "", e.to || "", e.key])
    const known = new Set()
    const onSync = o.onSync || (() => {})
    const onError = o.onError || (err => console.warn("cubby watch:", err))
    const hasDoc = typeof document !== "undefined"
    const hasWin = typeof window !== "undefined" && typeof window.addEventListener === "function"

    let stopped = false
    let controller = null
    let sub = null // the running watch on the live connection
    let hiddenSince = hasDoc && document.hidden ? Date.now() : null
    let interrupt = null // ends the current pause or retry wait early

    // Waits ms, or until woken. Without ms, waits until woken.
    const pause = ms => new Promise(r => {
      const timer = ms === undefined ? null : setTimeout(done, ms)
      function done() { clearTimeout(timer); interrupt = null; r() }
      interrupt = done
    })

    // The page came back (visible, online, restored from the back/forward
    // cache). Requests started before may hang on a dead connection, so start
    // a fresh one now. The version cursor makes that lossless.
    const wake = () => {
      if (stopped) return
      controller?.abort("wake")
      interrupt?.()
    }
    const onVisibility = () => {
      if (document.hidden) hiddenSince = Date.now()
      else { hiddenSince = null; wake() }
    }
    if (hasDoc) document.addEventListener("visibilitychange", onVisibility)
    if (hasWin) {
      window.addEventListener("online", wake)
      window.addEventListener("offline", wake)
      window.addEventListener("pageshow", wake)
    }
    // When the live connection opens, a watch waiting on HTTP moves over.
    const offOpen = live ? live.onOpen(() => controller?.abort("wake")) : null
    const cleanup = () => {
      if (hasDoc) document.removeEventListener("visibilitychange", onVisibility)
      if (hasWin) {
        window.removeEventListener("online", wake)
        window.removeEventListener("offline", wake)
        window.removeEventListener("pageshow", wake)
      }
      offOpen?.()
    }

    const safely = (fn, arg) => {
      try {
        fn(arg)
      } catch (err) {
        onError(err)
      }
    }
    const deliver = change => {
      if (change.deleted) known.delete(entryId(change))
      else known.add(entryId(change))
      safely(onChange, withMine({ deleted: false, ...change }))
    }

    let backoff = 1000
    let resync = null // keys seen in the snapshot after a 410, or true for a "now" watch
    let initial = true
    const retry = async err => {
      onError(err)
      await pause(err.code === "rate_limited" || err.status === 429 ? (err.retryAfter || 5) * 1000 : backoff)
      backoff = Math.min(backoff * 2, 30000)
    }
    // 410: the version is too old. Start over and report what vanished.
    const expired = () => {
      if (onlyNew) {
        from = "now"
        resync = true
      } else {
        from = 0
        resync = new Set()
      }
    }
    // One watch response, from an HTTP request or a live push.
    const handle = body => {
      backoff = 1000
      if (resync instanceof Set) for (const c of body.changes) resync.add(entryId(c))
      for (const c of body.changes) deliver(c)
      from = body.version
      if (body.more) return
      if (resync instanceof Set) {
        for (const id of [...known]) {
          if (resync.has(id)) continue
          const [by, from, to, key] = JSON.parse(id)
          deliver({ key, by: by || undefined, from: from || undefined, to: to || undefined, deleted: true, version: body.version })
        }
      }
      if (stopped) return
      safely(onSync, { version: body.version, initial, resync: !!resync })
      initial = false
      resync = null
    }

    // Watches over the live connection until it ends. false: the watch stops.
    const overLive = async () => {
      let cur
      try {
        cur = auth ? await ident.get() : await ident.peek()
      } catch (err) {
        await retry(err instanceof KvError ? err : new KvError(0, "network_error", String(err?.message || err)))
        return true
      }
      const path = await pathOf(target)
      if (stopped || !live.isOpen()) return true
      sub = live.subscribe(cur?.token, path + "/watch", { after: String(from) }, handle)
      const end = await sub.done
      sub = null
      if (!end.frame) return true // the connection closed, or stop()
      if (end.frame.status === 410) { expired(); return true }
      const err = await errorFrom(frameResponse(end.frame))
      if (err.code === "identity_changed") return true
      if (!err.retryable) {
        onError(err) // the watch stops here; err.retryable tells the app
        return false
      }
      await retry(err)
      return true
    }

    ;(async () => {
      let offlineReported = false
      while (!stopped) {
        if (typeof navigator !== "undefined" && navigator.onLine === false) {
          if (!offlineReported) { onError(new KvError(0, "network_error", "The browser is offline.")); offlineReported = true }
          await pause()
          continue
        }
        offlineReported = false
        if ((hiddenSince && Date.now() - hiddenSince > HIDDEN_PAUSE) || live?.paused) {
          await pause()
          continue
        }
        if (live && await live.ready(LIVE_WATCH_WAIT)) {
          if (stopped || !(await overLive())) break
          continue
        }
        if (stopped) break
        controller = new AbortController()
        const deadline = setTimeout(() => controller.abort("deadline"), WATCH_DEADLINE)
        let res, body
        try {
          res = await request("GET", (await pathOf(target)) + "/watch", {
            auth, signal: controller.signal, query: { after: from },
          })
          if (res.ok) body = await res.json()
        } catch (err) {
          clearTimeout(deadline)
          if (stopped) break
          if (controller.signal.reason === "wake") { backoff = 1000; continue }
          await retry(controller.signal.reason === "deadline"
            ? new KvError(0, "network_error", `No answer from the server in ${WATCH_DEADLINE / 1000} s.`)
            : err instanceof KvError ? err : new KvError(0, "network_error", String(err?.message || err)))
          continue
        }
        clearTimeout(deadline)
        if (stopped) break
        if (res.status === 410) {
          expired()
          continue
        }
        if (!res.ok) {
          const err = await fail(res)
          if (!err.retryable) {
            onError(err) // the watch stops here; err.retryable tells the app
            break
          }
          await retry(err)
          continue
        }
        handle(body)
      }
      cleanup()
    })()

    return () => {
      if (stopped) return
      stopped = true
      controller?.abort("stop")
      sub?.stop()
      interrupt?.()
    }
  }

  // Calls onPulse for each pulse sent to target from now on, until the
  // returned function is called. Over the live connection when it's open,
  // HTTP long polls otherwise; switching loses nothing still in the
  // server's short window. onMissed runs when some were dropped: the
  // listener fell more than a few seconds or 256 pulses behind.
  function listenPulses(a, target, onPulse, o = {}) {
    const { pathOf, auth } = a
    checkKey(target, true)
    checkOptions(o, ["onError", "onMissed"], "pulses()")
    const onError = o.onError || (err => console.warn("cubby pulses:", err))
    const onMissed = o.onMissed || (() => {})
    const hasDoc = typeof document !== "undefined"
    const hasWin = typeof window !== "undefined" && typeof window.addEventListener === "function"
    let stopped = false, controller = null, sub = null, interrupt = null, after
    let hiddenSince = hasDoc && document.hidden ? Date.now() : null
    let backoff = 1000

    const pause = ms => new Promise(r => {
      const timer = ms === undefined ? null : setTimeout(done, ms)
      function done() { clearTimeout(timer); interrupt = null; r() }
      interrupt = done
    })
    const wake = () => { if (!stopped) { controller?.abort("wake"); interrupt?.() } }
    const onVisibility = () => {
      if (document.hidden) hiddenSince = Date.now()
      else { hiddenSince = null; wake() }
    }
    if (hasDoc) document.addEventListener("visibilitychange", onVisibility)
    if (hasWin) for (const e of ["online", "offline", "pageshow"]) window.addEventListener(e, wake)
    const offOpen = live ? live.onOpen(() => controller?.abort("wake")) : null
    const safely = (fn, arg) => { try { fn(arg) } catch (err) { onError(err) } }
    const handle = body => {
      backoff = 1000
      if (body.missed) safely(onMissed)
      for (const p of body.pulses) {
        if (stopped) return
        safely(onPulse, withMine(p))
      }
      after = body.seq
    }
    const retry = async err => {
      onError(err)
      await pause(err.status === 429 ? (err.retryAfter || 1) * 1000 : backoff)
      backoff = Math.min(backoff * 2, 30000)
    }
    const query = () => after === undefined ? undefined : { after: String(after) }

    ;(async () => {
      while (!stopped) {
        if ((typeof navigator !== "undefined" && navigator.onLine === false) ||
            (hiddenSince && Date.now() - hiddenSince > HIDDEN_PAUSE) || live?.paused) {
          await pause()
          continue
        }
        const path = (await pathOf(target)) + "/pulse"
        if (live && await live.ready(LIVE_WATCH_WAIT)) {
          if (stopped) break
          let cur
          try { cur = auth ? await ident.get() : await ident.peek() } catch (err) { await retry(err instanceof KvError ? err : new KvError(0, "network_error", String(err?.message || err))); continue }
          if (!live.isOpen()) continue
          sub = live.subscribe(cur?.token, path, query(), handle)
          const end = await sub.done
          sub = null
          if (!end.frame) continue
          const err = await errorFrom(frameResponse(end.frame))
          if (err.code === "identity_changed") continue
          if (!err.retryable) { onError(err); break }
          await retry(err)
          continue
        }
        if (stopped) break
        controller = new AbortController()
        const deadline = setTimeout(() => controller.abort("deadline"), WATCH_DEADLINE)
        try {
          const res = await request("GET", path, { auth, signal: controller.signal, query: query() })
          clearTimeout(deadline)
          if (!res.ok) {
            const err = await fail(res)
            if (!err.retryable) { onError(err); break }
            await retry(err)
            continue
          }
          handle(await res.json())
        } catch (err) {
          clearTimeout(deadline)
          if (stopped) break
          if (controller.signal.reason === "wake") continue
          await retry(err instanceof KvError ? err : new KvError(0, "network_error", String(err?.message || err)))
        }
      }
      if (hasDoc) document.removeEventListener("visibilitychange", onVisibility)
      if (hasWin) for (const e of ["online", "offline", "pageshow"]) window.removeEventListener(e, wake)
      offOpen?.()
    })()

    return () => {
      if (stopped) return
      stopped = true
      controller?.abort("stop")
      sub?.stop()
      interrupt?.()
    }
  }

  const me = async () => "by/" + (await ident.get()).id
  const byAreas = new Map()

  // JSON calls on the space itself: open, settings, invites, members.
  async function call(method, path, { body, auth = true, query } = {}) {
    const res = await request(method, path, { body: body === undefined ? undefined : JSON.stringify(body), auth, query })
    if (!res.ok) throw await errorFrom(res)
    return res.json()
  }

  // The recipient's side of to/: read and delete only. owners: the owners'
  // shared mailbox.
  function inbox(owners) {
    const base = owners ? "inbox/owners/" : "inbox/"
    return sender => {
      if (sender === "*") return readOnly(makeArea(base + "*", { auth: true }), `inbox${owners ? ".owners" : ""}("*")`, "Read or delete one sender's with inbox(senderId).")
      if (sender !== "me") checkVisitor(sender)
      const all = areaMethods(makeArea(base + sender, { auth: true }))
      return { get: all.get, delete: all.delete, series: all.series, list: all.list, watch: all.watch, pulses: all.pulses }
    }
  }

  const db = {
    space,
    clock: makeClock(server, fetchFn),
    // Your level here, and what the space allows: { space, id, access,
    // expires, canInvite, settings, usage, limits, invite, can() }. It never
    // throws for "not invited": access is "none" for a missing space and a
    // private one alike. It redeems an invite first: o.invite, or
    // #invite=... in the address (removed once read). invite says how that
    // went: "redeemed", "invalid" or null for none.
    async open(o = {}) {
      checkOptions(o, ["invite"], "open()")
      const invite = "invite" in o ? o.invite : takeHashParam("invite")
      let outcome = null
      if (invite) {
        try {
          await call("POST", "join", { body: { invite } })
          outcome = "redeemed"
        } catch (err) {
          if (err.code !== "invite_invalid" && err.code !== "banned") throw err
          if (err.code === "invite_invalid") outcome = "invalid"
        }
      }
      try {
        return makeAccess({ ...(await call("GET", "", { auth: false })), invite: outcome })
      } catch (err) {
        if (err.code !== "no_space" && err.code !== "banned") throw err
        const banned = err.code === "banned"
        return makeAccess({ space, id: ident.visitorId() ?? null, access: banned ? "banned" : "none",
          expires: banned ? err.details.expires ?? null : null, canInvite: false, settings: null, usage: null, limits: null, invite: outcome })
      }
    },
    // Owners: change { anyone, inviters }; either may be left out. Resolves
    // with the settings now.
    settings: async patch => (await call("PATCH", "", { body: patch })).settings,
    // Owners: delete the space and everything in it.
    deleteSpace: async () => { await call("DELETE", "") },
    // All or nothing: up to 100 writes in one request and one transaction,
    // each { set: path, value }, { delete: path } or { add: path, value }
    // with ifVersion, ifBefore, ttl and t as on single writes. { check:
    // path, ifVersion } writes nothing and fails the batch if the key has
    // another version (0: if it exists). Paths are as
    // in the HTTP API: "shared/title", "by/me/cursor", "to/u_.../note".
    // Resolves { version, items: [{ path, version, created?, t? }] }. If one
    // write fails, nothing is written and its error has details.index.
    async batch(writes) {
      if (!Array.isArray(writes) || writes.length === 0) {
        throw new KvError(0, "bad_batch", "batch takes an array of 1 to 100 writes: { set: path, value }, { delete: path } or { add: path, value }.")
      }
      const body = writes.map(w => {
        const out = { ...w }
        for (const k of ["t", "ifBefore"]) if (out[k] !== undefined) out[k] = timeMs(out[k])
        return out
      })
      return call("POST", "batch", { body })
    },
    // Invites: owners, and members at the space's inviters level (post
    // unless changed). Members see and change only their own, and give at
    // most their own level.
    invites: {
      // { label, access, uses, ttl, accessTtl, base } → the invite and its
      // link: base (this page without its fragment) + #space=...&invite=...
      // Defaults: access all (or your level if lower), any number of uses,
      // 7 days. accessTtl ends the access it gives that long after redeeming.
      async create(o = {}) {
        checkOptions(o, ["label", "access", "uses", "ttl", "accessTtl", "base"], "invites.create()")
        const { base, ...body } = o
        const out = await call("POST", "invites", { body })
        return { ...out, link: inviteLink(base, space, out.invite) }
      },
      list: async () => (await call("GET", "invites")).items,
      // { uses, ttl }: how often and how long it can still be redeemed.
      update: (id, o = {}) => {
        checkOptions(o, ["uses", "ttl"], "invites.update()")
        return call("PATCH", "invites/" + encodeURIComponent(id), { body: o })
      },
      // Revokes it and every grant redeemed from it.
      revoke: async id => { await call("DELETE", "invites/" + encodeURIComponent(id)) },
    },
    members: {
      // Who is in the space: { id, access, since }, highest level first.
      // Owners also get expires, invitedBy, lastSeen, label, and bans.
      list: async () => (await call("GET", "members")).items,
      // Owners: give a visitor exactly one level, replacing what they had:
      // "read", "post", "all", "owner", or "banned" to keep them out and
      // close their live connections. o.ttl ends it.
      set(id, level, o = {}) {
        checkVisitor(id)
        checkOptions(o, ["ttl"], "members.set()")
        return call("PUT", "members/" + id, { body: { access: level, ...o } })
      },
      // Owners remove a visitor's grants and any ban; "me" leaves the space.
      remove: async id => {
        if (id !== "me") checkVisitor(id)
        return (await call("DELETE", "members/" + id)).removed
      },
    },
    shared: areaMethods(makeArea("shared")),
    // by("me"): your own keys, only you write them; by(id): someone's, to
    // read; by("*"): everyone's together, each item with by.
    by(who) {
      if (who === "*") return readOnly(makeArea("by/*", { byAuthor: true }), 'by("*")', 'Write your own keys with by("me").')
      if (!byAreas.has(who)) {
        if (who !== "me") checkVisitor(who)
        byAreas.set(who, areaMethods(who === "me" ? makeArea(me) : makeArea("by/" + who, { other: who })))
      }
      return byAreas.get(who)
    },
    // What you send one recipient: a visitor ID, "me" (private to you) or
    // "owners". One slot per recipient, sender and key, so senders never
    // overwrite each other. Only the recipient reads it, in their inbox, and
    // you read back what you sent. to("*"): everything you sent, items with to.
    to(who) {
      if (who === "*") return readOnly(makeArea("to/*", { auth: true }), 'to("*")', "Write to one recipient with to(id).")
      if (who !== "me" && who !== "owners") checkVisitor(who)
      return areaMethods(makeArea("to/" + who, { auth: true }))
    },
    // What others sent you. Items carry from and to; your own to("me")
    // entries are here too, from you. inbox("*") for everyone's, inbox(id)
    // to read and delete one sender's. inbox.owners(...): mail to the owners.
    inbox: Object.assign(inbox(false), { owners: inbox(true) }),
    // Who's here now, from pulses on your own key. See presence above.
    presence: (key, o) => presence(key, o),
    // status: "http" without live (or after falling back), else "connecting",
    // "open", "closed" (not needed right now, or between retries) or "offline".
    // Set onStatus to hear about changes. Nothing else depends on it.
    live: live ? live.api : { status: "http", onStatus: null },
  }
  return db
}


// A 404 for a key that doesn't exist, as opposed to no_space for a space
// this visitor can't see.
async function missingKey(res) {
  if (res.status !== 404) return false
  const b = await res.clone().json().catch(() => ({}))
  return b.error === "not_found"
}

async function errorFrom(res) {
  const b = await res.json().catch(() => ({}))
  return new KvError(res.status, b.error || "http_error", b.message || res.statusText, b)
}

// Unknown options are refused, so a typo never quietly does something else.
function checkOptions(o, known, where) {
  if (o === null || typeof o !== "object") throw new KvError(0, "invalid_option", `${where} takes an options object.`)
  for (const k of Object.keys(o)) {
    if (!known.includes(k)) {
      throw new KvError(0, "invalid_option", `Unknown option ${k} for ${where}. Options are ${known.filter(x => typeof x === "string").join(", ")}.`)
    }
  }
}

// A time: ms since 1970, or a Date.
function timeMs(v) {
  return v instanceof Date ? v.getTime() : v
}

// A duration in ms, from seconds or "90s", "10m", "12h", "14d".
function durationMs(v, name) {
  if (typeof v === "number" && v > 0) return v * 1000
  const m = typeof v === "string" && /^(\d+)([smhd]?)$/.exec(v)
  if (m && Number(m[1]) > 0) return Number(m[1]) * { "": 1, s: 1, m: 60, h: 3600, d: 86400 }[m[2]] * 1000
  throw new KvError(0, "invalid_option", `${name} is a duration: seconds, or a string like "90s", "10m", "12h" or "14d".`)
}

// Query parameters of a write, times as ms.
function writeQuery(o) {
  return { ifVersion: o.ifVersion, ttl: o.ttl, ifBefore: timeMs(o.ifBefore), t: timeMs(o.t) }
}

const LEVELS = ["banned", "none", "read", "post", "all", "owner"]

// me from open(): access is the visitor's level. can(op, area) answers
// from it: reading takes read, writing your own keys (by("me"), to) post,
// writing shared all.
function makeAccess(a) {
  a.can = (op = "read", area = "shared") => {
    const need = op === "read" ? "read" : area === "shared" ? "all" : "post"
    return LEVELS.indexOf(a.access) >= LEVELS.indexOf(need)
  }
  return a
}

function hashParams() {
  return new URLSearchParams(typeof location === "undefined" ? "" : location.hash.slice(1))
}

// Reads name=... from the address fragment and removes it, keeping the
// fragment's other parameters. null if there's none.
function takeHashParam(name) {
  const p = hashParams()
  const value = p.get(name)
  if (!value) return null
  p.delete(name)
  const rest = p.toString()
  if (typeof history !== "undefined") {
    history.replaceState(history.state, "", location.pathname + location.search + (rest ? "#" + rest : ""))
  }
  return value
}

// base (default: this page) with its fragment's other parameters and
// space=..., invite=... added.
function inviteLink(base, space, invite) {
  if (!base && typeof location === "undefined") return null
  const u = new URL(base || location.href)
  const p = new URLSearchParams(u.hash.slice(1))
  p.delete("invite"); p.delete("pair"); p.delete("space")
  p.set("space", space)
  p.set("invite", invite)
  u.hash = p.toString()
  return u.href
}

// pairUrl adds pair=code to base's fragment, keeping its other parameters
// and dropping an invite.
function pairUrl(code, base) {
  if (!base && typeof location === "undefined") return null
  const u = new URL(base || location.href)
  const p = new URLSearchParams(u.hash.slice(1))
  p.delete("invite")
  p.set("pair", code)
  u.hash = p.toString()
  return u.href
}

// A short guess at what this device is, like "Safari on iOS".
function deviceLabel() {
  const ua = globalThis.navigator?.userAgent || ""
  if (!ua || /^Node\.js/.test(ua)) return typeof process !== "undefined" && process.versions?.node ? "Node.js" : ""
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Browser"
  const os = /iPhone|iPad|iPod/.test(ua) ? "iOS" : /Android/.test(ua) ? "Android" : /Mac OS X/.test(ua) ? "macOS" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : ""
  return os ? browser + " on " + os : browser
}

// visitor: you, across spaces. Your ID, the spaces you own or were invited
// to, the devices that are you, and API keys for tools that can't sign.
export function visitor(opts = {}) {
  checkOptions(opts, ["server", "fetch", "keyName", "credentials"], "visitor()")
  const server = (opts.server || SERVER).replace(/\/$/, "")
  const fetchFn = opts.fetch || globalThis.fetch.bind(globalThis)
  const ident = identity(server, opts.keyName || "cubby:" + server, fetchFn, { credentials: opts.credentials })

  async function send(method, path, body, token, signal) {
    let res
    try {
      res = await fetchFn(server + path, { method, signal, headers: token ? { Authorization: "Bearer " + token } : {},
        body: body === undefined ? undefined : JSON.stringify(body) })
    } catch (err) {
      if (err?.name === "AbortError") throw err
      throw new KvError(0, "network_error", String(err?.message || err))
    }
    if (!res.ok) throw await errorFrom(res)
    return res.json()
  }
  // A signed call. peek: only with a key this device already has; null
  // without one, and nothing is registered.
  async function call(method, path, body, { signal, peek = false } = {}) {
    for (let attempt = 0; ; attempt++) {
      const cur = peek ? await ident.peek() : await ident.get()
      if (!cur) return null
      try {
        return await send(method, path, body, cur.token, signal)
      } catch (err) {
        if (attempt === 0 && (err.code === "token_expired" || err.code === "token_not_yet_valid")) { await ident.learnSkew(); continue }
        // This device was signed out: forget its visitor, so the next call
        // starts a new one.
        if (err.code === "unknown_key") {
          ident.forget()
          if (peek) return null
        }
        throw err
      }
    }
  }
  // POST /v1/pair, signed by signer with its key in the header.
  async function usePairingCode(signer, code, label) {
    return send("POST", "/v1/pair", { code: String(code), label }, await signer.token(ident.aud(), ident.now(), true, 300))
  }

  function makePairing(out, base) {
    const ctl = new AbortController()
    let done = null
    return {
      id: out.id, code: out.code, display: out.display, expires: out.expires,
      url: pairUrl(out.code, base),
      // Resolves with { device } once another device uses the code;
      // rejects with pairing_code_expired or pairing_code_cancelled.
      get done() {
        if (!done) done = (async () => {
          for (;;) {
            let l
            try {
              l = await call("GET", "/v1/me/pairing-codes/" + out.id + "?timeout=55", undefined, { signal: ctl.signal })
            } catch (err) {
              if (err?.name === "AbortError") l = await call("GET", "/v1/me/pairing-codes/" + out.id)
              else if (err instanceof KvError && err.retryable) { await sleep(2000); continue }
              else throw err
            }
            if (l.state === "used") return { device: l.device }
            if (l.state !== "open") throw new KvError(410, "pairing_code_" + l.state, l.state === "expired" ? "The code expired before another device used it." : "The code was cancelled.", l)
          }
        })()
        return done
      },
      async cancel() {
        const l = await call("DELETE", "/v1/me/pairing-codes/" + out.id)
        ctl.abort()
        return l.state
      },
    }
  }

  return {
    // This device's visitor ID. Makes one (registers this device) if it
    // has none yet.
    async id() { return (await ident.get()).id },
    // A space this visitor owns: { space, id, settings }. Private unless
    // anyone gives every visitor a level. prefix goes before the random
    // words; name is a whole name of your own.
    async createSpace(o = {}) {
      checkOptions(o, ["prefix", "name", "anyone", "inviters"], "createSpace()")
      return call("POST", "/v1/spaces", o)
    },
    // One page of the spaces you belong to (own or were invited to), the
    // same on every paired device, most recently active first:
    // { items, next }; pass next back as after. Each item: { space, access,
    // via, since, expires, members, lastActive, bytes, keys, settings }. A
    // device without a key gets an empty list and isn't registered.
    async spaces(o = {}) {
      checkOptions(o, ["limit", "after"], "spaces()")
      const q = new URLSearchParams()
      if (o.limit !== undefined) q.set("limit", String(o.limit))
      if (o.after !== undefined && o.after !== null) q.set("after", o.after)
      const qs = q.toString()
      return (await call("GET", "/v1/spaces" + (qs ? "?" + qs : ""), undefined, { peek: true })) ?? { items: [], next: null }
    },
    // The code in #pair=..., without using it. null if there's none.
    pending: () => hashParams().get("pair"),
    // A one-use code for another device, alive ttl (30s to 1h, default
    // 10m). url is this page with #pair=CODE, or base's.
    async pair(o = {}) {
      checkOptions(o, ["ttl", "base"], "pair()")
      const out = await call("POST", "/v1/me/pairing-codes", o.ttl === undefined ? {} : { ttl: o.ttl })
      return makePairing(out, o.base)
    },
    // Use a code: the one given, or #pair=... from the address, which is
    // removed. This device becomes that visitor; a previous identity with
    // no other device is deleted with its data. null if there's no code.
    async accept(code, o = {}) {
      checkOptions(o, ["label"], "accept()")
      if (code == null) code = takeHashParam("pair")
      if (code == null) return null
      const out = await usePairingCode(await ident.signer(), code, o.label ?? deviceLabel())
      await ident.adopt(out.id)
      return out
    },
    // A backup key: a new device of this visitor whose private key is
    // returned as text, once, and kept nowhere here. Anyone with the text
    // can act as this visitor, so it belongs in a password manager.
    async backup(o = {}) {
      checkOptions(o, ["label"], "backup()")
      const code = await call("POST", "/v1/me/pairing-codes", { ttl: 60 })
      const k = await newKeyPair(true)
      const signer = await makeSigner(k.privateKey, k.jwk)
      const out = await usePairingCode(signer, code.code, o.label ?? "Backup key " + new Date().toISOString().slice(0, 10))
      const key = await crypto.subtle.exportKey("jwk", k.privateKey)
      return JSON.stringify({ server, visitor: out.id, device: out.device, key: { kty: key.kty, crv: key.crv, x: key.x, y: key.y, d: key.d } })
    },
    // Restore from a backup key: the backup makes a pairing code and this
    // device uses it, so this device becomes that visitor and the backup
    // stays a backup.
    async restore(backup, o = {}) {
      checkOptions(o, ["label"], "restore()")
      const signer = await importCredentials(backup)
      const code = await send("POST", "/v1/me/pairing-codes", { ttl: 60 }, await signer.token(ident.aud(), ident.now()))
      const out = await usePairingCode(await ident.signer(), code.code, o.label ?? deviceLabel())
      await ident.adopt(out.id)
      return out
    },
    devices: {
      // This visitor's devices, oldest first. current marks this one.
      list: async () => (await call("GET", "/v1/me/devices")).items,
      rename: async (label, id = "current") => (await call("PATCH", "/v1/me/devices/" + id, { label })).items,
      // Sign a device out: its key stops working. Not the last one.
      remove: async id => (await call("DELETE", "/v1/me/devices/" + id)).removed,
      removeOthers: async () => (await call("DELETE", "/v1/me/devices?others=1")).removed,
    },
    // API keys: static secrets for tools that can't sign, like webhooks. One
    // space each, at most your level (all or yours, as an invite), 30 days
    // unless ttl says otherwise (null: never, the weakest choice).
    apiKeys: {
      create(space, o = {}) {
        checkOptions(o, ["label", "access", "ttl"], "apiKeys.create()")
        return call("POST", "/v1/me/api-keys", { space, ...o })
      },
      list: async () => (await call("GET", "/v1/me/api-keys")).items,
      revoke: async id => { await call("DELETE", "/v1/me/api-keys/" + encodeURIComponent(id)) },
    },
  }
}

// A reply frame from the live connection as a fetch Response, so every
// caller reads it exactly as it reads HTTP.
function frameResponse(f) {
  const headers = new Headers(f.headers || {})
  let body = null
  if (f.body !== undefined) {
    body = f.text ? f.body : JSON.stringify(f.body)
    headers.set("Content-Type", f.text ? "text/plain; charset=utf-8" : "application/json; charset=utf-8")
  }
  return new Response(body, { status: f.status, headers })
}

// The live connection for kv(space, { live: true }): one WebSocket, opened
// on first use. Requests use it only while it's open; until then, and
// whenever it's down, they go over HTTP. Watches move to it when it opens
// and resume from their version after it closes, so nothing is lost. It
// never resends a request. If it can't connect twice before ever opening (a
// proxy that blocks WebSockets, an older server), it stays on HTTP.
function makeLive(url, WS) {
  const hasDoc = typeof document !== "undefined"
  const hasWin = typeof window !== "undefined" && typeof window.addEventListener === "function"
  const isOffline = () => typeof navigator !== "undefined" && navigator.onLine === false
  let sock = null, status = "closed", everOpened = false, failures = 0
  let nextId = 1, sentToken
  let backoff = 0, notBefore = 0, retryTimer = null, pingTimer = null, hiddenTimer = null
  let paused = false // hidden long enough that the socket was closed
  const pending = new Map(), subs = new Map(), waiters = new Set(), openers = new Set()
  const wills = new Map() // path -> { token, body }, registered again on every open

  const api = { onStatus: null, get status() { return status } }
  const setStatus = next => {
    if (next === status) return
    status = next
    try { api.onStatus?.(next) } catch (err) { console.warn("cubby live:", err) }
  }
  const settle = () => { for (const w of [...waiters]) w() }

  function connect() {
    if (sock || status === "http" || paused || isOffline() || retryTimer) return
    const wait = notBefore - Date.now()
    if (wait > 0) {
      retryTimer = setTimeout(() => { retryTimer = null; connect() }, wait)
      return
    }
    let s
    try {
      s = new WS(url)
    } catch {
      failed()
      return
    }
    sock = s
    setStatus("connecting")
    const openTimer = setTimeout(() => drop(s), LIVE_OPEN_WAIT)
    s.onopen = () => {
      clearTimeout(openTimer)
      everOpened = true
      failures = 0
      backoff = 0
      sentToken = undefined
      setStatus("open")
      schedulePing()
      for (const [path, w] of wills) sendWill(path, w).catch(err => console.warn("cubby will:", err))
      settle()
      for (const fn of [...openers]) fn()
    }
    s.onmessage = e => {
      let f
      try { f = JSON.parse(e.data) } catch { return }
      const p = pending.get(f.id)
      if (p) {
        pending.delete(f.id)
        p.resolve(f)
        return
      }
      subs.get(f.id)?.frame(f)
    }
    s.onclose = () => { clearTimeout(openTimer); drop(s) }
    s.onerror = () => {} // onclose follows
  }

  function failed() {
    if (!everOpened && !isOffline() && ++failures >= 2) {
      setStatus("http")
      settle()
      return true
    }
    backoff = Math.min(Math.max(backoff * 2, 1000), 30000)
    notBefore = Date.now() + backoff
    return false
  }

  // Ends socket s now, whatever state it's in. A dead connection can take
  // minutes to report close, so nothing waits for the browser.
  function drop(s) {
    if (sock !== s) return
    sock = null
    s.onopen = s.onmessage = s.onclose = s.onerror = null
    try { s.close(1000) } catch {}
    clearTimeout(pingTimer)
    const wasOpen = status === "open"
    for (const p of pending.values()) p.reject(new Error("The live connection closed."))
    pending.clear()
    for (const sub of subs.values()) sub.end({ closed: true })
    subs.clear()
    if (wasOpen) {
      backoff = 0
      notBefore = 0
    } else if (failed()) {
      return
    }
    setStatus(isOffline() ? "offline" : "closed")
    settle()
  }

  function send(build, wait) {
    const s = sock
    return new Promise((resolve, reject) => {
      if (!s || s.readyState !== 1) return reject(new Error("The live connection isn't open."))
      const id = nextId++
      const timer = setTimeout(() => {
        pending.delete(id)
        reject(new Error("No answer on the live connection."))
        drop(s)
      }, wait)
      pending.set(id, {
        resolve: f => { clearTimeout(timer); resolve(f) },
        reject: err => { clearTimeout(timer); reject(err) },
      })
      try {
        s.send(build(id))
      } catch (err) {
        pending.get(id)?.reject(err)
        pending.delete(id)
      }
    })
  }

  function schedulePing() {
    clearTimeout(pingTimer)
    pingTimer = setTimeout(() => ping(LIVE_PING_WAIT), LIVE_PING)
  }
  function ping(wait) {
    const s = sock
    if (!s || status !== "open") return
    send(id => JSON.stringify({ id, op: "ping" }), wait).then(() => { if (sock === s) schedulePing() }, () => {})
  }

  // The server takes the visitor from the latest auth message. Sent only
  // when it changes; the reply isn't needed, since a bad token shows up on
  // the requests after it exactly as on HTTP.
  function authAs(token) {
    const t = token || null
    if (t === sentToken) return
    sentToken = t
    sock.send(JSON.stringify({ id: nextId++, op: "auth", token: t }))
  }

  function sendWill(path, w) {
    authAs(w.token)
    return send(id => {
      let m = `{"id":${id},"op":"will","path":${JSON.stringify(path)}`
      if (w.body !== undefined) m += `,"body":${w.body}`
      return m + "}"
    }, LIVE_REQUEST_WAIT).then(frameResponse)
  }

  if (hasDoc) {
    document.addEventListener("visibilitychange", () => {
      clearTimeout(hiddenTimer)
      if (document.hidden) {
        hiddenTimer = setTimeout(() => { paused = true; if (sock) drop(sock) }, HIDDEN_PAUSE)
      } else {
        paused = false
        if (status === "open") ping(LIVE_WAKE_PING_WAIT) // after a phone sleeps the socket may be dead
      }
    })
  }
  if (hasWin) {
    window.addEventListener("offline", () => { if (sock) drop(sock); else if (status !== "http") setStatus("offline") })
    window.addEventListener("online", () => {
      notBefore = 0
      clearTimeout(retryTimer)
      retryTimer = null
      if (status === "offline") setStatus("closed")
    })
    // An open socket keeps the page out of the back/forward cache, so it
    // closes on pagehide and stays closed until the page shows again.
    window.addEventListener("pagehide", () => { paused = true; if (sock) drop(sock) })
    window.addEventListener("pageshow", () => { if (!(hasDoc && document.hidden)) paused = false })
  }

  return {
    api,
    get paused() { return paused },
    isOpen: () => status === "open" && sock?.readyState === 1,
    ensure: connect,
    onOpen(fn) { openers.add(fn); return () => openers.delete(fn) },
    // A last will: the server sends it as a pulse when this socket ends.
    // Kept here and registered again whenever a socket opens. Resolves with
    // the server's answer, or null when no socket is open yet.
    async setWill(token, path, body) {
      wills.set(path, { token, body })
      connect()
      if (status !== "open") return null
      return sendWill(path, { token, body })
    },
    clearWill(path) {
      if (!wills.delete(path) || status !== "open") return
      sendWill(path, { token: sentToken }).catch(() => {})
    },
    // Resolves true once open, false if it isn't open within ms.
    ready(ms) {
      if (status === "open") return Promise.resolve(true)
      connect()
      if (!sock && !retryTimer) return Promise.resolve(false)
      return new Promise(resolve => {
        const done = () => { clearTimeout(timer); waiters.delete(done); resolve(status === "open") }
        const timer = setTimeout(done, ms)
        waiters.add(done)
      })
    },
    request(token, method, path, query, body) {
      if (!sock) return Promise.reject(new Error("The live connection isn't open."))
      authAs(token)
      return send(id => {
        let m = `{"id":${id},"method":${JSON.stringify(method)},"path":${JSON.stringify(path)}`
        if (Object.keys(query).length) m += `,"query":${JSON.stringify(query)}`
        if (body !== undefined) m += `,"body":${body}` // kv.js bodies are JSON text, sent byte for byte
        return m + "}"
      }, LIVE_REQUEST_WAIT).then(frameResponse)
    },
    // A running request on the socket: a watch or a pulse listener. Pushes
    // each body to onBody. done resolves with { frame } for an error reply,
    // { closed } when the socket ends, { stopped } after stop().
    subscribe(token, path, query, onBody) {
      const s = sock
      let finish
      const done = new Promise(r => { finish = r })
      authAs(token)
      const id = nextId++
      subs.set(id, {
        frame: f => {
          if (f.status === 200) onBody(f.body)
          else { subs.delete(id); finish({ frame: f }) }
        },
        end: finish,
      })
      const m = { id, method: "GET", path }
      if (query) m.query = query
      s.send(JSON.stringify(m))
      return {
        done,
        stop() {
          if (!subs.delete(id)) return
          try { if (sock === s) s.send(JSON.stringify({ op: "stop", watch: id })) } catch {}
          finish({ stopped: true })
        },
      }
    },
  }
}

// Clock samples do not send tokens or create a visitor. now() uses a monotonic
// clock after sync, so changing the computer's wall clock cannot shift a timer.
function makeClock(server, fetchFn) {
  const mono = () => globalThis.performance?.now() ?? Date.now()
  let anchor = null, offset = null, rtt = null, pending = null, listening = false, epoch = 0
  const timers = new Set()
  const hasWin = typeof window !== "undefined" && typeof window.addEventListener === "function"
  const hasDoc = typeof document !== "undefined"
  const refresh = () => { if (!hasDoc || !document.hidden) clock.sync().catch(() => {}) }
  const clock = {
    get offset() { return offset },
    get rtt() { return rtt },
    async sync({ samples = 5 } = {}) {
      if (!Number.isInteger(samples) || samples < 1 || samples > 20) throw new KvError(0, "invalid_option", "samples is 1 to 20.")
      if (pending) return pending
      const generation = epoch
      pending = (async () => {
        let best = null, lastError
        for (let i = 0; i < samples; i++) {
          const wall = Date.now(), sent = mono()
          try {
            const res = await fetchFn(server + "/v1/time", { cache: "no-store" })
            if (!res.ok) throw await errorFrom(res)
            const body = await res.json(), received = mono(), trip = received - sent
            if (!Number.isSafeInteger(body.t)) throw new KvError(0, "bad_response", "The server did not return a time.")
            if (!best || trip < best.rtt) best = { rtt: trip, offset: body.t - wall - trip / 2, server: body.t + trip / 2, local: received }
          } catch (err) { lastError = err instanceof KvError ? err : new KvError(0, "network_error", String(err?.message || err)) }
        }
        if (!best) throw lastError
        if (generation !== epoch) return { offset: best.offset, rtt: best.rtt }
        anchor = best; offset = best.offset; rtt = best.rtt
        if (!listening) {
          if (hasWin) window.addEventListener("online", refresh)
          if (hasDoc) document.addEventListener("visibilitychange", refresh)
          listening = true
        }
        for (const timer of timers) timer.arm()
        return { offset, rtt }
      })().finally(() => { pending = null })
      return pending
    },
    now() {
      if (!anchor) throw new KvError(0, "clock_not_synced", "Call db.clock.sync() first.")
      return anchor.server + mono() - anchor.local
    },
    at(t, fn) {
      if (!Number.isSafeInteger(t) || typeof fn !== "function") throw new KvError(0, "invalid_option", "at takes server milliseconds and a function.")
      clock.now()
      let handle
      const timer = { arm() {
        clearTimeout(handle)
        const delay = t - clock.now()
        handle = setTimeout(() => {
          if (t > clock.now()) { timer.arm(); return }
          timers.delete(timer); fn()
        }, Math.min(2147483647, Math.max(0, Math.ceil(delay))))
      } }
      timer.cancel = () => { clearTimeout(handle); timers.delete(timer) }
      timers.add(timer); timer.arm()
      return timer.cancel
    },
    dispose() {
      epoch++
      if (hasWin) window.removeEventListener("online", refresh)
      if (hasDoc) document.removeEventListener("visibilitychange", refresh)
      listening = false
      for (const timer of timers) timer.cancel?.()
      timers.clear()
    },
  }
  return clock
}
