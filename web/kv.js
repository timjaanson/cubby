// cubby client. A guide for whoever writes code with this, person or coding agent.
//
// Spaces are PRIVATE by default. Only the creator (the owner) and people they
// invite can read or write. Everyone else gets 404 no_space, exactly as if the
// space didn't exist, so a missing invite looks like a wrong name. Check both.
//
// Every page starts the same way:
//   const db = kv(SPACE)
//   const me = await db.open()     // redeems an invite link (#t=...), says what you may do
//   if (!me.inside) { /* show: "ask the owner for an invite link" */ }
//
// Creating a space (once from a shell with `cubby new`, or per room in the page):
//   const { space, ownerToken } = await createSpace()
//   Show ownerToken to the user once. It's their recovery key. Never store it in code.
// A page that is a room, shared by its link? openRoom("board") creates or finds
// the space, opens it and gives await room.link(). Members can share it too.
//
// Sharing with specific people, the usual case:
//   const { link } = await db.share({ label: "Anna" })       // member, 10 uses, 7 days
//   const { link } = await db.share({ allow: "viewer", uses: 1 })
//   Ready-made UI with link, QR code and member list: import "<server>/share.js"
//   and put <cubby-share space="..."></cubby-share> in the page.
//
// Make a space public only when the user asked for it:
//   createSpace({ public: "read" })     anyone reads, invited people write: status pages
//   createSpace({ public: "respond" })  anyone reads and adds their own entry: polls, RSVPs
//   createSpace({ public: "all" })      anyone with the name reads and writes
//
// Lots of live interaction (a shared board, a quiz, game state)? Use
//   kv(SPACE, { live: true })
// Same API and results; data and watches go over one WebSocket, falling back
// to HTTP by itself. Write limits stay the same.
//
// Cursors, drags, reactions? Pulses are values that are never stored:
//   db.user().pulse("cursor", { x, y })              // to whoever listens now
//   db.users.onPulse("cursor", p => draw(p.user, p.value))
// Anything a late joiner needs still goes in a stored key. Who's here:
//   db.users.presence("here", { value: { name } })   // .peers, .onJoin, .onLeave
//
// Never put an access token (at_...) or owner token (ot_...) in page source.
// Anyone who can load the page could use it. Invite links are for sending to people.
//
// Docs: docs/agents.md in the cubby repository.
//
// An ES module with no dependencies. Exports: kv, createSpace, openRoom,
// takeInvite, inviteLink, newName, validSpace, KvError.

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
export const validSpace = name => typeof name === "string" && name.length >= 16 && name.length <= 63 && SPACE_RE.test(name)

// newName returns the label (cleaned up) and four random words, 48 random bits.
export function newName(label = "") {
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

// One identity per server, shared by every kv() instance on the page.
const identities = new Map()

function identity(server, tokenKey, fetchFn, fixedToken) {
  if (fixedToken) {
    // A visitor token given by the caller: used as is, never saved or renewed.
    const v = { token: fixedToken, id: null }
    return {
      fixed: true, current: () => v, adopt() {},
      async get() {
        if (!v.id) {
          const res = await fetchFn(server + "/v1/whoami?token=" + encodeURIComponent(fixedToken))
          const body = await res.json().catch(() => ({}))
          if (!res.ok) throw new KvError(res.status, body.error || "http_error", body.message || res.statusText, body)
          v.id = body.id
        }
        return v
      },
      renew: () => { throw new KvError(401, "invalid_token", "The given visitor token is unknown or expired.") },
    }
  }
  const id = server + "\n" + tokenKey
  if (identities.has(id)) return identities.get(id)
  let memory = null
  let warned = false
  const store = {
    load() {
      try {
        const raw = localStorage.getItem(tokenKey)
        const v = raw ? JSON.parse(raw) : memory
        // Only visitor tokens count; anything else (an older kind) is dropped.
        return typeof v?.token === "string" && v.token.startsWith("vt_") ? v : null
      } catch {
        return memory
      }
    },
    save(v) {
      memory = v
      try {
        if (v) localStorage.setItem(tokenKey, JSON.stringify(v))
        else localStorage.removeItem(tokenKey)
      } catch {
        if (!warned) {
          warned = true
          console.warn("cubby: localStorage isn't available, so this visitor is new on every page load")
        }
      }
    },
  }
  let pending = null
  const ident = {
    current: () => store.load(),
    async get() {
      const cur = store.load()
      if (cur?.token) return cur
      return ident.renew()
    },
    // A visitor the server created in a createSpace or join response.
    adopt(token, id) {
      if (token) store.save({ token, id })
    },
    // The server doesn't know the stored token. Calls that can create a
    // visitor (createSpace, join) go on without one.
    forget() {
      store.save(null)
    },
    renew() {
      if (!pending) {
        pending = (async () => {
          const res = await fetchFn(server + "/v1/anon", { method: "POST" })
          const body = await res.json().catch(() => ({}))
          if (!res.ok) throw new KvError(res.status, body.error || "http_error", body.message || res.statusText, body)
          const v = { token: body.token, id: body.id }
          store.save(v)
          return v
        })().finally(() => { pending = null })
      }
      return pending
    },
  }
  identities.set(id, ident)
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
      "The newest value per key is always sent. Pass { maxRate } to pulse() or pulseRate to kv() to change the limit.")
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
  if (!validSpace(space)) {
    const suggestion = newName(String(space ?? ""))
    throw new KvError(0, "invalid_space", `"${space}" isn't a valid space name. Try ${suggestion}.`, { suggestion })
  }
  const server = (opts.server || SERVER).replace(/\/$/, "")
  const fetchFn = opts.fetch || globalThis.fetch.bind(globalThis)
  const ident = identity(server, opts.tokenKey || "cubby:" + server, fetchFn, opts.token)
  const WS = opts.WebSocket || globalThis.WebSocket
  const pulseRate = opts.pulseRate ?? 20
  const live = opts.live && typeof WS === "function" ? makeLive(server.replace(/^http/, "ws") + "/v1/" + space + "/live", WS) : null

  // auth: the request needs a visitor token, and gets one if there isn't one
  // yet. Without auth, whatever token there is still goes along, so a
  // private space works with no option. Reads send it as ?token=: an
  // Authorization header makes the browser send a CORS preflight, cached per
  // URL, and every watch request has a new URL.
  async function request(method, path, { body, auth = false, query, signal } = {}) {
    const read = method === "GET" || method === "HEAD"
    for (let attempt = 0; ; attempt++) {
      const cur = auth ? await ident.get() : ident.current()
      let url = `${server}/v1/${space}${path ? "/" + path : ""}`
      const q = new URLSearchParams()
      for (const [k, v] of Object.entries(query || {})) if (v !== undefined && v !== null) q.set(k, String(v))
      if (read && cur?.token) q.set("token", cur.token)
      const qs = q.toString()
      if (qs) url += "?" + qs
      const headers = {}
      if (!read && cur?.token) headers.Authorization = "Bearer " + cur.token
      const dataPath = path.startsWith("shared/") || path.startsWith("me/") || path.startsWith("users/")
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
        if (b.error === "invalid_token") {
          // The visitor is gone, say after a year unused. The new one has no
          // grants, so in a private space the retry gets no_space. Without
          // auth (join), go on with no token and let the server make one.
          if (auth) await ident.renew()
          else ident.forget()
          continue
        }
      }
      if (dataPath && !read && (method !== "POST" || path.endsWith("/series"))) {
        const header = res.headers.get("RateLimit")
        db.writeBudget = header ? parseBudget(header) : null
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
  function throttledPulse(path, body, rate, report = true) {
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
  const withMine = e => ({ ...e, mine: !!e.by && e.by === ident.current()?.id })

  // other: for another visitor's area, the ID only they may write under.
  // Owners may still delete there; the server decides that.
  function area(prefix, { needsAuth = false, other = null } = {}) {
    const checkWrite = () => {
      if (other && ident.current()?.id !== other) throw new KvError(0, "not_yours", "This is another visitor's area. Only they can write it.")
    }
    const pathOf = async target => {
      const p = typeof prefix === "function" ? await prefix() : prefix
      return `${p}/${target}`
    }

    const a = {
      async getEntry(key) {
        checkKey(key)
        const res = await request("GET", await pathOf(key), { auth: needsAuth })
        if (await missingKey(res)) return null
        if (!res.ok) throw await fail(res)
        const version = Number((res.headers.get("ETag") || "").replace(/"/g, ""))
        const text = await res.text()
        const isJSON = (res.headers.get("Content-Type") || "").startsWith("application/json")
        const entry = { value: isJSON ? JSON.parse(text) : text, version, by: res.headers.get("KV-By") || undefined }
        const t = res.headers.get("KV-Time")
        if (t) entry.t = Number(t)
        const expires = res.headers.get("KV-Expires")
        if (expires) entry.expires = Number(expires)
        return withMine(entry)
      },
      async get(key) {
        const e = await a.getEntry(key)
        return e ? e.value : null
      },
      async set(key, value, o = {}) {
        checkKey(key)
        checkWrite()
        const body = JSON.stringify(value)
        if (body === undefined) throw new KvError(0, "invalid_value", "That value can't be stored as JSON.")
        const res = await request("PUT", await pathOf(key), { body, auth: true, query: { ifVersion: o.ifVersion, ttl: o.ttl, ifBefore: o.ifBefore } })
        if (!res.ok) throw await fail(res)
        return (await res.json()).version
      },
      async delete(key, o = {}) {
        checkKey(key)
        const res = await request("DELETE", await pathOf(key), { auth: true, query: { ifVersion: o.ifVersion, ifBefore: o.ifBefore } })
        if (await missingKey(res)) return false
        if (!res.ok) throw await fail(res)
        return true
      },
      async update(key, fn, o = {}) {
        const retries = o.retries ?? 10
        for (let i = 0; ; i++) {
          const cur = await a.getEntry(key)
          const next = fn(cur ? cur.value : null)
          if (next === undefined || (cur && JSON.stringify(next) === JSON.stringify(cur.value))) return cur ? cur.value : null
          try {
            await a.set(key, next, { ...o, ifVersion: cur ? cur.version : 0 })
            return next
          } catch (err) {
            if (err.code !== "version_conflict" || i >= retries) throw err
            await sleep(50 + Math.random() * 150)
          }
        }
      },
      // add("temp", 21.4) adds one point; value can be any JSON value, an
      // object is one point. add("env.*", { temp: 21.4, rh: 45 }) adds one
      // point to env.temp and one to env.rh. The target decides, never the value.
      async add(target, value, o = {}) {
        checkKey(target, true)
        checkWrite()
        const batch = target.endsWith("*")
        if (batch && (value === null || typeof value !== "object" || Array.isArray(value))) {
          throw new KvError(0, "batch_needs_object", `Adding to ${target} takes an object like { temp: 21.4 }; each property becomes a point in its own series.`)
        }
        const body = JSON.stringify(value)
        if (body === undefined) throw new KvError(0, "invalid_value", "That value can't be stored as JSON.")
        const res = await request("POST", (await pathOf(target)) + "/series", { body, auth: true, query: { t: o.t, ifBefore: o.ifBefore } })
        if (!res.ok) throw await fail(res)
        return res.json()
      },
      // One request. Points come oldest first; pass next back as since for more.
      // null if the key doesn't exist.
      async series(key, o = {}) {
        checkKey(key)
        const res = await request("GET", (await pathOf(key)) + "/series", { auth: needsAuth, query: { since: o.since, limit: o.limit } })
        if (await missingKey(res)) return null
        if (!res.ok) throw await fail(res)
        return res.json()
      },
      list: (target, o) => list(pathOf, needsAuth, target, o),
      watch: (target, onChange, o) => watch(pathOf, needsAuth, target, onChange, o),
      // Pulses are never stored. pulse() hands a value to whoever is
      // listening right now; nothing keeps it, so get() and watch() never
      // see it. For cursors, drags, reactions: things sent often, or that
      // matter as events rather than as state.
      //
      // At most maxRate per second per key (default 20, kv option pulseRate).
      // Faster sends wait for the next slot, and a newer value replaces one
      // still waiting, so the latest always goes out. Resolves true when
      // sent, false when replaced. maxRate: 0 sends everything at once.
      async pulse(key, value, o = {}) {
        checkKey(key)
        checkWrite()
        const body = JSON.stringify(value)
        if (body === undefined) throw new KvError(0, "invalid_value", "That value can't be sent as JSON.")
        const path = (await pathOf(key)) + "/pulse"
        return throttledPulse(path, body, o.maxRate ?? pulseRate, !o[QUIET])
      },
      onPulse: (target, onPulse, o) => listenPulses(pathOf, needsAuth, target, onPulse, o),
      // A last will: a pulse the server sends for you when your live
      // connection ends without you clearing it, such as a closed tab. Only
      // with live: true; over HTTP it's kept and does nothing, so the same
      // code works either way. Resolves with a function that clears it.
      async will(key, value) {
        checkKey(key)
        checkWrite()
        const body = JSON.stringify(value)
        if (body === undefined) throw new KvError(0, "invalid_value", "That value can't be sent as JSON.")
        const path = (await pathOf(key)) + "/pulse"
        if (!live) return () => {}
        const res = await live.setWill((await ident.get()).token, path, body)
        if (res && !res.ok) throw await fail(res)
        return () => live.clearWill(path)
      },
    }
    return a
  }

  // Who else is here, from pulses on users/{id}/{key}: a heartbeat every
  // interval with your value, a leave pulse on stop() and pagehide, and a
  // last will with the same leave on a live connection, so a closed tab
  // leaves at once. A peer silent for timeout counts as gone, which is how
  // leaving shows over HTTP. peers never includes you.
  function presence(key, o = {}) {
    checkKey(key)
    const interval = o.interval ?? 2000
    const timeout = o.timeout ?? interval * 3
    const warn = o.onError || (err => console.warn("cubby presence:", err))
    const hasWin = typeof window !== "undefined" && typeof window.addEventListener === "function"
    const mine = db.user()
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
    const stopListen = db.users.onPulse(key, p => {
      if (p.mine || !p.user || stopped) return
      const v = p.value
      if (v && typeof v === "object" && v.left === true) return leave(p.user)
      if (!v || typeof v !== "object" || !("v" in v)) return // not a presence pulse
      const now = Date.now(), cur = peers.get(p.user)
      if (!cur) {
        peers.set(p.user, { value: v.v, since: now, seen: now })
        call(h.onJoin, p.user, v.v)
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

  async function list(pathOf, auth, target, o = {}) {
    checkKey(target, true)
    const limit = o.limit ?? Infinity
    const items = []
    let after, version
    for (;;) {
      const res = await request("GET", await pathOf(target), {
        auth, query: { limit: Math.min(1000, limit - items.length), order: o.order, after },
      })
      if (!res.ok) throw await fail(res)
      const body = await res.json()
      if (version === undefined) version = body.version
      items.push(...body.items.map(withMine))
      if (!body.next || items.length >= limit) return { items, version }
      after = body.next
    }
  }

  const entryId = e => (e.user || "") + "/" + e.key

  // from:
  //   0 (default)   every existing key first, then changes
  //   "now"         only changes from now on
  //   a list result continue right after that list
  //   a version     continue after that version
  // After a 410 (the version is too old, say after a week asleep) a watch with
  // state starts again from 0 and reports keys it knew that are gone as
  // deleted. It knows the keys it has delivered, plus a list's items. A "now"
  // watch skips ahead to now instead. Either way the next onSync has resync: true.
  function watch(pathOf, auth, target, onChange, o = {}) {
    checkKey(target, true)
    const fromList = o.from !== null && typeof o.from === "object"
    if (fromList && !(Array.isArray(o.from.items) && Number.isSafeInteger(o.from.version))) {
      throw new KvError(0, "invalid_option", "from takes 0, \"now\", a version, or a result of list().")
    }
    let from = fromList ? o.from.version : (o.from ?? 0)
    if (from !== "now" && !(Number.isSafeInteger(from) && from >= 0)) {
      throw new KvError(0, "invalid_option", "from takes 0, \"now\", a version, or a result of list().")
    }
    const onlyNew = from === "now"
    const known = new Set(fromList ? o.from.items.map(entryId) : [])
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
          const slash = id.indexOf("/")
          deliver({ key: id.slice(slash + 1), user: id.slice(0, slash) || undefined, deleted: true, version: body.version })
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
        cur = auth ? await ident.get() : ident.current()
      } catch (err) {
        await retry(err instanceof KvError ? err : new KvError(0, "network_error", String(err?.message || err)))
        return true
      }
      const path = await pathOf(target)
      if (stopped || !live.isOpen()) return true
      sub = live.subscribe(cur?.token, path + "/watch", from === "now" ? undefined : { version: String(from) }, handle)
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
            auth, signal: controller.signal, query: { version: from === "now" ? undefined : from },
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
  function listenPulses(pathOf, auth, target, onPulse, o = {}) {
    checkKey(target, true)
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
          try { cur = auth ? await ident.get() : ident.current() } catch (err) { await retry(err instanceof KvError ? err : new KvError(0, "network_error", String(err?.message || err))); continue }
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

  const myArea = async () => "users/" + (await ident.get()).id

  // JSON calls on the space itself: join, access, settings, tokens, grants.
  async function call(method, path, { body, auth = true, query } = {}) {
    const res = await request(method, path, { body: body === undefined ? undefined : JSON.stringify(body), auth, query })
    if (!res.ok) throw await fail(res)
    return res.json()
  }

  const accessFrom = (a, extra = {}) => makeAccess({ inside: true, joined: false, inviteFailed: false, ...a, ...extra })

  const db = {
    writeBudget: null, // latest response snapshot; never a promise of future capacity
    clock: makeClock(server, fetchFn),
    async limits() {
      const { limits, usage } = await db.access()
      return { limits, usage }
    },
    async join(token) {
      if (token === null || token === undefined || token === "") return null
      // Without a stored visitor, the server creates one and returns it.
      const out = await call("POST", "join", { body: { token }, auth: false })
      ident.adopt(out.token, out.id)
      return db.access()
    },
    async access() {
      return accessFrom(await call("GET", "access", { auth: false }))
    },
    // The first call on every page. It never throws for "not invited":
    // inside is false for a missing space and for a private one alike.
    async open(o = {}) {
      const invite = "invite" in o ? o.invite : takeInvite()
      let joined = false
      let inviteFailed = false
      if (invite) {
        try {
          await db.join(invite)
          joined = true
        } catch (err) {
          if (err.code !== "invite_invalid") throw err
          inviteFailed = true
        }
      }
      try {
        return accessFrom(await call("GET", "access", { auth: false }), { joined, inviteFailed })
      } catch (err) {
        if (err.code !== "no_space") throw err
        return makeAccess({ inside: false, joined, inviteFailed, id: ident.current()?.id ?? null, owner: false, canInvite: false, settings: null, grants: [] })
      }
    },
    // An invite link to send: this page (or base) with #t=at_... added.
    async share(o = {}) {
      const { base, ...rest } = o
      const t = await db.tokens.create(rest)
      return { link: inviteLink(t.token, base), token: t.token, id: t.id, allow: t.allow, uses: t.uses, until: t.until }
    },
    // Invites. Owners, and members unless the space says invite: "owners".
    // Members see and change only their own, and give at most what they hold.
    tokens: {
      create: (o = {}) => call("POST", "tokens", { body: tokenBody(o, true) }),
      list: async () => (await call("GET", "tokens")).items,
      update: (id, o = {}) => call("PATCH", "tokens/" + encodeURIComponent(id), { body: tokenBody(o, false) }),
      revoke: async id => { await call("DELETE", "tokens/" + encodeURIComponent(id)) },
    },
    owner: {
      settings: async patch => (await call("PATCH", "", { body: patch })).settings,
      deleteSpace: async () => { await call("DELETE", "") },
      rotateOwnerToken: async () => (await call("POST", "owner-token")).owner,
      grants: {
        list: async (o = {}) => (await call("GET", "grants", { query: { user: o.user } })).items,
        add: (user, o = {}) => {
          const body = { user }
          if (o.owner) body.owner = true
          else if (o.allow !== undefined) body.allow = o.allow
          if (o.until !== undefined) body.until = timeValue(o.until)
          return call("POST", "grants", { body })
        },
        revoke: async id => { await call("DELETE", "grants/" + encodeURIComponent(id)) },
        revokeUser: async user => { await call("DELETE", "grants", { query: { user } }) },
      },
    },
    shared: area("shared"),
    me: area("me", { needsAuth: true }),
    user(id) {
      if (id === undefined) return area(myArea)
      if (typeof id !== "string" || !VISITOR_RE.test(id)) {
        throw new KvError(0, "invalid_key", `${JSON.stringify(id)} isn't a visitor ID. IDs look like u_k7m2x9q4p1r8t3w6.`)
      }
      return area("users/" + id, { other: id })
    },
    users: {
      list: (target, o) => list(async t => `users/*/${t}`, false, target, o),
      watch: (target, onChange, o) => watch(async t => `users/*/${t}`, false, target, onChange, o),
      onPulse: (target, onPulse, o) => listenPulses(async t => `users/*/${t}`, false, target, onPulse, o),
      presence: (key, o) => presence(key, o),
    },
    id: async () => (await ident.get()).id,
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

// until and grantFor take a duration string ("7d"), a Date, milliseconds
// since 1970, or null.
function timeValue(v) {
  if (v instanceof Date) return v.toISOString()
  return v
}

function tokenBody(o, creating) {
  const known = creating ? ["label", "allow", "uses", "until", "grantFor"] : ["uses", "until"]
  const body = {}
  for (const k of Object.keys(o)) {
    if (!known.includes(k)) throw new KvError(0, "invalid_option", `Unknown option ${k}. Options are ${known.join(", ")}.`)
  }
  for (const k of known) if (k in o) body[k] = k === "until" ? timeValue(o[k]) : o[k]
  if (typeof body.grantFor === "number") body.grantFor = Math.max(1, Math.round(body.grantFor / 1000)) + "s"
  return body
}

// A scope is AREA[/TARGET]:RIGHTS. covers mirrors the server's rule.
function covers(scope, method, area, key) {
  const [body, rights] = scope.split(":")
  if (!rights || !rights.includes(method === "write" ? "w" : "r")) return false
  const slash = body.indexOf("/")
  const sArea = slash < 0 ? body : body.slice(0, slash)
  const target = slash < 0 ? "*" : body.slice(slash + 1)
  if (sArea !== "*" && sArea !== area) return false
  if (key === undefined) return target === "*"
  if (target.endsWith("*")) return key.replace(/\*$/, "").startsWith(target.slice(0, -1))
  return !key.endsWith("*") && key === target
}

function makeAccess(a) {
  a.grants = a.grants || []
  a.can = (method, area, key) => {
    if (!a.inside) return false
    if (a.owner) return true
    const s = a.settings
    if (method === "read" && s?.read === "anyone") return true
    if (method === "write" && s?.write?.[area] === "anyone") return true
    return a.grants.some(g => g.owner || g.allow.some(sc => covers(sc, method, area, key)))
  }
  return a
}

// createSpace makes a space owned by this visitor, private unless
// public says otherwise. Show ownerToken to the person once: it's their
// recovery key. It isn't stored.
export async function createSpace(o = {}) {
  const { server: srv, tokenKey, fetch: f, ...fields } = o
  const server = (srv || SERVER).replace(/\/$/, "")
  const fetchFn = f || globalThis.fetch.bind(globalThis)
  const ident = identity(server, tokenKey || "cubby:" + server, fetchFn)
  for (const k of Object.keys(fields)) {
    if (!["label", "name", "public", "read", "write", "invite"].includes(k)) {
      throw new KvError(0, "invalid_option", `Unknown option ${k}. Options are label, name, public, read, write, invite.`)
    }
  }
  let res
  for (let attempt = 0; ; attempt++) {
    const headers = {}
    const cur = ident.current()
    if (cur?.token) headers.Authorization = "Bearer " + cur.token
    try {
      res = await fetchFn(server + "/v1/spaces", { method: "POST", headers, body: JSON.stringify(fields) })
    } catch (err) {
      throw new KvError(0, "network_error", String(err?.message || err))
    }
    // A stored visitor the server no longer knows: create the space as a
    // new visitor instead.
    if (res.status === 401 && cur?.token && attempt === 0) {
      const b = await res.clone().json().catch(() => ({}))
      if (b.error === "invalid_token") { ident.forget(); continue }
    }
    break
  }
  if (!res.ok) throw await errorFrom(res)
  const out = await res.json()
  ident.adopt(out.token, out.id)
  return { space: out.space, ownerToken: out.owner }
}

function hashParams() {
  return new URLSearchParams(typeof location === "undefined" ? "" : location.hash.slice(1))
}

// takeInvite reads #t=... from the address bar and removes it, keeping the
// fragment's other parameters. null if there's none.
export function takeInvite(param = "t") {
  const p = hashParams()
  const token = p.get(param)
  if (!token) return null
  p.delete(param)
  const rest = p.toString()
  if (typeof history !== "undefined") {
    history.replaceState(history.state, "", location.pathname + location.search + (rest ? "#" + rest : ""))
  }
  return token
}

// openRoom: a page is a room, and its link lets others in. Finds the room
// in the address (#s=...) or the one this browser was last in under label,
// creates one if there's neither, opens it (redeeming #t=...), and keeps
// #s=<space> in the address so a reload stays in the room.
//   const room = await openRoom("board", { live: true })
//   if (!room.me.inside) ...           // gone, or a bad link
//   const link = await room.link()     // a reusable invite, minted once per device
// Options: param ("s"), create (true), space and invite (instead of the
// address, outside browsers), public, and anything kv() takes. Members can share too, so every device makes its own link.
export async function openRoom(label, o = {}) {
  const { param = "s", create = true, space: given, public: pub, invite, ...kvOpts } = o
  const server = (kvOpts.server || SERVER).replace(/\/$/, "")
  const key = `cubby-room:${server}:${label}`
  const fromHash = hashParams().get(param)
  let space = given || (fromHash && validSpace(fromHash) ? fromHash : null) || roomStore.get(key)
  let created = false
  if (!space) {
    if (!create) return { db: null, me: { inside: false }, space: null, created, link: async () => null, forget }
    const fields = { label, server: kvOpts.server, tokenKey: kvOpts.tokenKey, fetch: kvOpts.fetch }
    if (pub) fields.public = pub
    space = (await createSpace(fields)).space
    created = true
  }
  const db = kv(space, kvOpts)
  const me = await db.open(invite === undefined ? {} : { invite })
  if (me.inside) {
    roomStore.set(key, space)
    setHashParam(param, space)
  }
  // The invite this visitor minted for the room, reused while it still works.
  async function link(lo = {}) {
    const base = lo.base || (typeof location === "undefined" ? undefined : location.origin + location.pathname + location.search + "#" + param + "=" + space)
    const lkey = `cubby-room-link:${server}:${space}:${me.id || await db.id()}`
    const saved = roomStore.get(lkey)
    if (saved) {
      const t = await db.tokens.list().then(l => l.find(t => t.id === saved.id), () => null)
      if (t?.state === "active") return inviteLink(saved.token, base)
    }
    const t = await db.tokens.create({ label: label + " link", uses: null, until: null })
    roomStore.set(lkey, { id: t.id, token: t.token })
    return inviteLink(t.token, base)
  }
  function forget() {
    roomStore.del(key)
    setHashParam(param, null)
  }
  return { db, me, space, created, link, forget }
}

// localStorage when there is one, memory otherwise.
const roomStore = (() => {
  const mem = new Map()
  const ls = () => { try { return globalThis.localStorage || null } catch { return null } }
  return {
    get(k) { const l = ls(); const v = l ? l.getItem(k) : mem.get(k); try { return v ? JSON.parse(v) : null } catch { return null } },
    set(k, v) { const j = JSON.stringify(v); const l = ls(); try { l ? l.setItem(k, j) : mem.set(k, j) } catch { mem.set(k, j) } },
    del(k) { const l = ls(); try { l?.removeItem(k) } catch {} mem.delete(k) },
  }
})()

function setHashParam(name, value) {
  if (typeof location === "undefined" || typeof history === "undefined") return
  const p = hashParams()
  if (value === null) p.delete(name)
  else p.set(name, value)
  const rest = p.toString()
  history.replaceState(history.state, "", location.pathname + location.search + (rest ? "#" + rest : ""))
}

// inviteLink adds t=token to base's fragment, keeping its other parameters.
export function inviteLink(token, base = typeof location === "undefined" ? "" : location.href) {
  const u = new URL(base)
  const p = new URLSearchParams(u.hash.slice(1))
  p.set("t", token)
  u.hash = p.toString()
  return u.href
}

function parseBudget(header) {
  const buckets = {}
  for (const member of header.split(",")) {
    const name = member.match(/^\s*"([^"]+)"/)
    const remaining = member.match(/;\s*r=(\d+)/)
    if (name && remaining) buckets[name[1]] = { remaining: Number(remaining[1]) }
  }
  return { ...buckets, observedAt: Date.now() }
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
