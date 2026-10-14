// tiny-kv JS client. An ES module with no dependencies.
//
//   import { kv } from "https://kv.example.com/kv.js"
//   const db = kv("poll-k7m2x9q4")
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
}

const SPACE_RE = /^([a-z0-9]+(-[a-z0-9]+)*-)?[a-z0-9]{8}$/
const ALNUM = "abcdefghijklmnopqrstuvwxyz0123456789"

function validSpace(name) {
  if (typeof name !== "string" || name.length > 63 || !SPACE_RE.test(name)) return false
  const suffix = name.slice(-8)
  const digits = suffix.replace(/[^0-9]/g, "").length
  return digits >= 2 && 8 - digits >= 2
}

function newName(label = "") {
  label = String(label).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 54).replace(/-+$/, "")
  for (;;) {
    const bytes = crypto.getRandomValues(new Uint8Array(8))
    const suffix = Array.from(bytes, b => ALNUM[b % 36]).join("")
    const name = label ? `${label}-${suffix}` : suffix
    if (validSpace(name)) return name
  }
}

const sleep = ms => new Promise(r => setTimeout(r, ms))

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
          console.warn("tiny-kv: localStorage isn't available, so this visitor is new on every page load")
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
  const ident = identity(server, opts.tokenKey || "tiny-kv:" + server, fetchFn)
  const spaceKey = opts.key || null
  let knownLocked = false

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
      const cur = auth ? await ident.get() : ident.current()
      if (cur?.token) headers.Authorization = "Bearer " + cur.token
      if (shared && spaceKey && method !== "GET") headers["Space-Key"] = spaceKey
      let res
      try {
        res = await fetchFn(url, { method, headers, body, signal })
      } catch (err) {
        if (err?.name === "AbortError") throw err
        throw new KvError(0, "network_error", String(err?.message || err))
      }
      if (res.status === 401 && attempt === 0) {
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

  function area(prefix, { needsAuth = false, readOnly = false, shared = false } = {}) {
    const checkWrite = () => {
      if (readOnly) throw new KvError(0, "read_only", "This is another visitor's area. Only they can write it.")
      if (shared && knownLocked && !spaceKey) throw new KvError(0, "space_locked", "This space is locked. Pass { key } to kv() to write shared/.")
    }
    const pathOf = async target => {
      const p = typeof prefix === "function" ? await prefix() : prefix
      return `${p}/${target}`
    }

    const a = {
      async getEntry(key) {
        const res = await request("GET", await pathOf(key), { auth: needsAuth })
        if (res.status === 404) return null
        if (!res.ok) throw await fail(res)
        const version = Number((res.headers.get("ETag") || "").replace(/"/g, ""))
        const text = await res.text()
        const isJSON = (res.headers.get("Content-Type") || "").startsWith("application/json")
        return { value: isJSON ? JSON.parse(text) : text, version }
      },
      async get(key) {
        const e = await a.getEntry(key)
        return e ? e.value : null
      },
      async set(key, value, o = {}) {
        checkWrite()
        const body = JSON.stringify(value)
        if (body === undefined) throw new KvError(0, "invalid_value", "That value can't be stored as JSON.")
        const res = await request("PUT", await pathOf(key), { body, auth: true, shared, query: { ifVersion: o.ifVersion } })
        if (!res.ok) throw await fail(res)
        return (await res.json()).version
      },
      async delete(key, o = {}) {
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
      list: (target, o) => list(pathOf, needsAuth, target, o),
      watch: (target, onChange, o) => watch(pathOf, needsAuth, target, onChange, o),
    }
    return a
  }

  async function list(pathOf, auth, target, o = {}) {
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
      items.push(...body.items)
      if (!body.next || items.length >= limit) return { items, version }
      after = body.next
    }
  }

  function watch(pathOf, auth, target, onChange, o = {}) {
    let stopped = false
    let controller = null
    let from = o.from === undefined ? 0 : o.from
    const known = new Set()
    const onError = o.onError || (err => console.warn("tiny-kv watch:", err))
    let hiddenSince = null
    let wake = null

    const onVisibility = () => {
      if (typeof document === "undefined") return
      if (document.hidden) hiddenSince = Date.now()
      else {
        hiddenSince = null
        wake?.()
      }
    }
    if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility)

    const deliver = change => {
      const id = (change.user || "") + "/" + change.key
      if (change.deleted) known.delete(id)
      else known.add(id)
      try {
        onChange({ deleted: false, ...change })
      } catch (err) {
        onError(err)
      }
    }

    ;(async () => {
      let backoff = 1000
      let resync = null
      while (!stopped) {
        if (hiddenSince && Date.now() - hiddenSince > 5 * 60 * 1000) {
          await new Promise(r => { wake = r })
          wake = null
          continue
        }
        controller = new AbortController()
        let res
        try {
          res = await request("GET", (await pathOf(target)) + "/watch", {
            auth, signal: controller.signal, query: { version: from === "now" ? undefined : from },
          })
        } catch (err) {
          if (stopped) return
          onError(err)
          await sleep(backoff)
          backoff = Math.min(backoff * 2, 30000)
          continue
        }
        if (res.status === 410) {
          from = 0
          resync = new Set()
          continue
        }
        if (res.status === 429) {
          const b = await res.json().catch(() => ({}))
          await sleep((b.retryAfter || 5) * 1000)
          continue
        }
        if (!res.ok) {
          const err = await fail(res)
          onError(err)
          if (res.status < 500) return
          await sleep(backoff)
          backoff = Math.min(backoff * 2, 30000)
          continue
        }
        backoff = 1000
        let body
        try {
          body = await res.json()
        } catch (err) {
          if (stopped) return
          onError(err)
          continue
        }
        if (stopped) return
        if (resync) {
          for (const c of body.changes) resync.add((c.user || "") + "/" + c.key)
        }
        for (const c of body.changes) deliver(c)
        if (resync && !body.more) {
          for (const id of [...known]) {
            if (!resync.has(id)) {
              const [user, key] = [id.slice(0, id.indexOf("/")), id.slice(id.indexOf("/") + 1)]
              deliver({ key, user: user || undefined, deleted: true, version: body.version })
            }
          }
          resync = null
        }
        from = body.version
      }
    })()

    return () => {
      stopped = true
      controller?.abort()
      wake?.()
      if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibility)
    }
  }

  const myArea = async () => "users/" + (await ident.get()).id

  return {
    shared: area("shared", { shared: true }),
    me: area("me", { needsAuth: true }),
    user(id) {
      if (id === undefined) return area(myArea)
      const cur = ident.current()
      return area("users/" + id, { readOnly: !cur || cur.id !== id })
    },
    users: {
      list: (target, o) => list(async t => `users/*/${t}`, false, target, o),
      watch: (target, onChange, o) => watch(async t => `users/*/${t}`, false, target, onChange, o),
    },
    id: async () => (await ident.get()).id,
  }
}

kv.newName = newName
export default kv
