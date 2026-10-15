// cubby JS client. An ES module with no dependencies.
//
//   import { kv, newName, validSpace, KvError } from "https://kv.example.com/kv.js"
//   const db = kv("poll-cedar-pilot-mango-frost")
//   await db.shared.set("question", "Pizza or tacos?")

const SERVER = new URL(".", import.meta.url).href.replace(/\/$/, "")

export class KvError extends Error {
  constructor(status, code, message, details = {}) {
    super(message)
    this.name = "KvError"
    this.status = status
    this.code = code
    this.details = details
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

// "watch" and "series" can follow a key in a path, so they aren't keys.
const validKey = k => typeof k === "string" && KEY_RE.test(k) && k !== "watch" && k !== "series" && k !== "." && k !== ".."
const validTarget = t => validKey(t) || (typeof t === "string" && PATTERN_RE.test(t))

// Checked before any request. Keys go into the URL, where "#", "?", "/" or
// ".." would silently address something else.
function checkKey(k, patternOk = false) {
  if (patternOk ? validTarget(k) : validKey(k)) return
  throw new KvError(0, "invalid_key",
    `${JSON.stringify(k)} isn't a valid ${patternOk ? "key or pattern" : "key"}. Keys are 1 to 128 of A-Z a-z 0-9 . _ : - and can't be ".", "..", "watch" or "series".` +
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

// One identity per server, shared by every kv() instance on the page.
const identities = new Map()

function identity(server, tokenKey, fetchFn) {
  const id = server + "\n" + tokenKey
  if (identities.has(id)) return identities.get(id)
  let memory = null
  let warned = false
  const store = {
    load() {
      try {
        const raw = localStorage.getItem(tokenKey)
        return raw ? JSON.parse(raw) : memory
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

export function kv(space, opts = {}) {
  if (!validSpace(space)) {
    const suggestion = newName(String(space ?? ""))
    throw new KvError(0, "invalid_space", `"${space}" isn't a valid space name. Try ${suggestion}.`, { suggestion })
  }
  const server = (opts.server || SERVER).replace(/\/$/, "")
  const fetchFn = opts.fetch || globalThis.fetch.bind(globalThis)
  const ident = identity(server, opts.tokenKey || "cubby:" + server, fetchFn)
  const spaceKey = opts.key || null
  let knownLocked = false

  // auth: the request needs the visitor's token, and gets one if there isn't
  // one yet. Public reads go without it: an Authorization header makes the
  // browser send a CORS preflight, and preflights are cached per URL, so every
  // watch request would pay for one.
  async function request(method, path, { body, auth = false, shared = false, query, signal } = {}) {
    let url = `${server}/v1/${space}/${path}`
    if (query) {
      const q = new URLSearchParams()
      for (const [k, v] of Object.entries(query)) if (v !== undefined && v !== null) q.set(k, String(v))
      const s = q.toString()
      if (s) url += "?" + s
    }
    for (let attempt = 0; ; attempt++) {
      const headers = {}
      if (auth) headers.Authorization = "Bearer " + (await ident.get()).token
      if (shared && spaceKey && method !== "GET") headers["Space-Key"] = spaceKey
      let res
      try {
        res = await fetchFn(url, { method, headers, body, signal })
      } catch (err) {
        if (err?.name === "AbortError") throw err
        throw new KvError(0, "network_error", String(err?.message || err))
      }
      if (auth && res.status === 401 && attempt === 0) {
        const b = await res.clone().json().catch(() => ({}))
        if (b.error === "invalid_token") {
          await ident.renew()
          continue
        }
      }
      return res
    }
  }

  async function fail(res) {
    const b = await res.json().catch(() => ({}))
    if (b.error === "space_locked") knownLocked = true
    return new KvError(res.status, b.error || "http_error", b.message || res.statusText, b)
  }

  // mine: this visitor made the last write or the delete.
  const withMine = e => ({ ...e, mine: !!e.by && e.by === ident.current()?.id })

  // owner: for another visitor's area, the ID only they may write under.
  function area(prefix, { needsAuth = false, owner = null, shared = false } = {}) {
    const checkWrite = () => {
      if (owner && ident.current()?.id !== owner) throw new KvError(0, "read_only", "This is another visitor's area. Only they can write it.")
      if (shared && knownLocked && !spaceKey) throw new KvError(0, "space_locked", "This space is locked. Pass { key } to kv() to write shared/.")
    }
    const pathOf = async target => {
      const p = typeof prefix === "function" ? await prefix() : prefix
      return `${p}/${target}`
    }

    const a = {
      async getEntry(key) {
        checkKey(key)
        const res = await request("GET", await pathOf(key), { auth: needsAuth })
        if (res.status === 404) return null
        if (!res.ok) throw await fail(res)
        const version = Number((res.headers.get("ETag") || "").replace(/"/g, ""))
        const text = await res.text()
        const isJSON = (res.headers.get("Content-Type") || "").startsWith("application/json")
        const entry = { value: isJSON ? JSON.parse(text) : text, version, by: res.headers.get("KV-By") || undefined }
        const t = res.headers.get("KV-Time")
        if (t) entry.t = Number(t)
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
        const res = await request("PUT", await pathOf(key), { body, auth: true, shared, query: { ifVersion: o.ifVersion } })
        if (!res.ok) throw await fail(res)
        return (await res.json()).version
      },
      async delete(key, o = {}) {
        checkKey(key)
        checkWrite()
        const res = await request("DELETE", await pathOf(key), { auth: true, shared, query: { ifVersion: o.ifVersion } })
        if (res.status === 404) return false
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
            await a.set(key, next, { ifVersion: cur ? cur.version : 0 })
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
        const res = await request("POST", (await pathOf(target)) + "/series", { body, auth: true, shared, query: { t: o.t } })
        if (!res.ok) throw await fail(res)
        return res.json()
      },
      // One request. Points come oldest first; pass next back as since for more.
      // null if the key doesn't exist.
      async series(key, o = {}) {
        checkKey(key)
        const res = await request("GET", (await pathOf(key)) + "/series", { auth: needsAuth, query: { since: o.since, limit: o.limit } })
        if (res.status === 404) return null
        if (!res.ok) throw await fail(res)
        return res.json()
      },
      list: (target, o) => list(pathOf, needsAuth, target, o),
      watch: (target, onChange, o) => watch(pathOf, needsAuth, target, onChange, o),
    }
    return a
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
      window.addEventListener("pageshow", wake)
    }
    const cleanup = () => {
      if (hasDoc) document.removeEventListener("visibilitychange", onVisibility)
      if (hasWin) {
        window.removeEventListener("online", wake)
        window.removeEventListener("pageshow", wake)
      }
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

    ;(async () => {
      let backoff = 1000
      let resync = null // keys seen in the snapshot after a 410, or true for a "now" watch
      let initial = true
      const retry = async err => {
        onError(err)
        await pause(err.code === "rate_limited" || err.status === 429 ? (err.details.retryAfter || 5) * 1000 : backoff)
        backoff = Math.min(backoff * 2, 30000)
      }
      while (!stopped) {
        if (hiddenSince && Date.now() - hiddenSince > HIDDEN_PAUSE) {
          await pause()
          continue
        }
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
          if (onlyNew) {
            from = "now"
            resync = true
          } else {
            from = 0
            resync = new Set()
          }
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
        backoff = 1000
        if (resync instanceof Set) for (const c of body.changes) resync.add(entryId(c))
        for (const c of body.changes) deliver(c)
        from = body.version
        if (body.more) continue
        if (resync instanceof Set) {
          for (const id of [...known]) {
            if (resync.has(id)) continue
            const slash = id.indexOf("/")
            deliver({ key: id.slice(slash + 1), user: id.slice(0, slash) || undefined, deleted: true, version: body.version })
          }
        }
        if (stopped) break
        safely(onSync, { version: body.version, initial, resync: !!resync })
        initial = false
        resync = null
      }
      cleanup()
    })()

    return () => {
      if (stopped) return
      stopped = true
      controller?.abort("stop")
      interrupt?.()
    }
  }

  const myArea = async () => "users/" + (await ident.get()).id

  return {
    shared: area("shared", { shared: true }),
    me: area("me", { needsAuth: true }),
    user(id) {
      if (id === undefined) return area(myArea)
      if (typeof id !== "string" || !VISITOR_RE.test(id)) {
        throw new KvError(0, "invalid_key", `${JSON.stringify(id)} isn't a visitor ID. IDs look like u_k7m2x9q4p1r8t3w6.`)
      }
      return area("users/" + id, { owner: id })
    },
    users: {
      list: (target, o) => list(async t => `users/*/${t}`, false, target, o),
      watch: (target, onChange, o) => watch(async t => `users/*/${t}`, false, target, onChange, o),
    },
    id: async () => (await ident.get()).id,
  }
}
