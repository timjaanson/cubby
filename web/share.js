// cubby share panel. A custom element that gives a space's owners invite
// links, a QR code, the member list, who can see the space, and its
// recovery key. Members can see a "shared with you" line; everyone else
// sees nothing.
//
//   <script type="module" src="https://kv.example.com/share.js"></script>
//   <cubby-share space="family-cedar-pilot-mango-frost" members></cubby-share>
//
// After createSpace(), set el.ownerToken = ownerToken and the panel opens
// once at the recovery key step, so the person saves it.
//
// Headless: import { qrSvg } from ".../share.js" for an SVG QR code of any text.

import { kv, inviteLink } from "./kv.js"

// ---------- QR code: byte mode, error correction M, versions 1 to 40 ----------

// Error correction codewords per block and number of blocks, level M.
const ECC_PER_BLOCK = [0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28]
const ECC_BLOCKS = [0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49]

function rawModules(ver) {
  let n = (16 * ver + 128) * ver + 64
  if (ver >= 2) {
    const align = Math.floor(ver / 7) + 2
    n -= (25 * align - 10) * align - 55
    if (ver >= 7) n -= 36
  }
  return n
}
const dataCodewords = ver => Math.floor(rawModules(ver) / 8) - ECC_PER_BLOCK[ver] * ECC_BLOCKS[ver]

function gfMul(x, y) {
  let z = 0
  for (let i = 7; i >= 0; i--) {
    z = (z << 1) ^ ((z >>> 7) * 0x11d)
    z ^= ((y >>> i) & 1) * x
  }
  return z
}

function rsDivisor(degree) {
  const r = new Array(degree).fill(0)
  r[degree - 1] = 1
  let root = 1
  for (let i = 0; i < degree; i++) {
    for (let j = 0; j < r.length; j++) {
      r[j] = gfMul(r[j], root)
      if (j + 1 < r.length) r[j] ^= r[j + 1]
    }
    root = gfMul(root, 2)
  }
  return r
}

function rsRemainder(data, div) {
  const r = new Array(div.length).fill(0)
  for (const b of data) {
    const f = b ^ r.shift()
    r.push(0)
    div.forEach((c, i) => { r[i] ^= gfMul(c, f) })
  }
  return r
}

function alignPositions(ver, size) {
  if (ver === 1) return []
  const n = Math.floor(ver / 7) + 2
  const step = ver === 32 ? 26 : Math.ceil((ver * 4 + 4) / (n * 2 - 2)) * 2
  const out = [6]
  for (let p = size - 7; out.length < n; p -= step) out.splice(1, 0, p)
  return out
}

// Returns a square matrix of booleans, true for dark.
export function qrMatrix(text) {
  const bytes = new TextEncoder().encode(text)
  let ver = 1
  for (; ver <= 40; ver++) {
    const bits = 4 + (ver < 10 ? 8 : 16) + bytes.length * 8
    if (bits <= dataCodewords(ver) * 8) break
  }
  if (ver > 40) throw new Error("Too long for a QR code")
  const size = ver * 4 + 17

  // Data bits, terminator, padding.
  const bits = []
  const put = (v, n) => { for (let i = n - 1; i >= 0; i--) bits.push((v >>> i) & 1) }
  put(4, 4)
  put(bytes.length, ver < 10 ? 8 : 16)
  for (const b of bytes) put(b, 8)
  const cap = dataCodewords(ver) * 8
  put(0, Math.min(4, cap - bits.length))
  put(0, (8 - bits.length % 8) % 8)
  for (let pad = 0xec; bits.length < cap; pad ^= 0xec ^ 0x11) put(pad, 8)
  const data = []
  for (let i = 0; i < bits.length; i += 8) data.push(bits.slice(i, i + 8).reduce((a, b) => (a << 1) | b, 0))

  // Blocks with error correction, interleaved.
  const nb = ECC_BLOCKS[ver], eccLen = ECC_PER_BLOCK[ver]
  const raw = Math.floor(rawModules(ver) / 8)
  const nShort = nb - raw % nb, shortLen = Math.floor(raw / nb)
  const div = rsDivisor(eccLen)
  const blocks = []
  for (let i = 0, k = 0; i < nb; i++) {
    const d = data.slice(k, k + shortLen - eccLen + (i < nShort ? 0 : 1))
    k += d.length
    const ecc = rsRemainder(d, div)
    if (i < nShort) d.push(0)
    blocks.push(d.concat(ecc))
  }
  const words = []
  for (let i = 0; i < blocks[0].length; i++) {
    blocks.forEach((b, j) => { if (i !== shortLen - eccLen || j >= nShort) words.push(b[i]) })
  }

  // Function patterns.
  const m = Array.from({ length: size }, () => new Array(size).fill(false))
  const fn = Array.from({ length: size }, () => new Array(size).fill(false))
  const set = (x, y, dark) => { m[y][x] = dark; fn[y][x] = true }
  for (let i = 0; i < size; i++) { set(6, i, i % 2 === 0); set(i, 6, i % 2 === 0) }
  for (const [cx, cy] of [[3, 3], [size - 4, 3], [3, size - 4]]) {
    for (let dy = -4; dy <= 4; dy++) for (let dx = -4; dx <= 4; dx++) {
      const x = cx + dx, y = cy + dy, d = Math.max(Math.abs(dx), Math.abs(dy))
      if (x >= 0 && x < size && y >= 0 && y < size) set(x, y, d !== 2 && d !== 4)
    }
  }
  const al = alignPositions(ver, size), last = al.length - 1
  al.forEach((ax, i) => al.forEach((ay, j) => {
    if ((i === 0 && j === 0) || (i === 0 && j === last) || (i === last && j === 0)) return
    for (let dy = -2; dy <= 2; dy++) for (let dx = -2; dx <= 2; dx++) set(ax + dx, ay + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1)
  }))
  const formatBits = mask => {
    const d = mask // level M is 00
    let r = d
    for (let i = 0; i < 10; i++) r = (r << 1) ^ ((r >>> 9) * 0x537)
    const b = ((d << 10) | r) ^ 0x5412
    const bit = i => ((b >>> i) & 1) === 1
    for (let i = 0; i <= 5; i++) set(8, i, bit(i))
    set(8, 7, bit(6)); set(8, 8, bit(7)); set(7, 8, bit(8))
    for (let i = 9; i < 15; i++) set(14 - i, 8, bit(i))
    for (let i = 0; i < 8; i++) set(size - 1 - i, 8, bit(i))
    for (let i = 8; i < 15; i++) set(8, size - 15 + i, bit(i))
    set(8, size - 8, true)
  }
  formatBits(0)
  if (ver >= 7) {
    let r = ver
    for (let i = 0; i < 12; i++) r = (r << 1) ^ ((r >>> 11) * 0x1f25)
    const b = (ver << 12) | r
    for (let i = 0; i < 18; i++) {
      const dark = ((b >>> i) & 1) === 1, a = size - 11 + i % 3, c = Math.floor(i / 3)
      set(a, c, dark); set(c, a, dark)
    }
  }

  // Codewords in the zigzag.
  let i = 0
  for (let right = size - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5
    for (let v = 0; v < size; v++) for (let j = 0; j < 2; j++) {
      const x = right - j, up = ((right + 1) & 2) === 0, y = up ? size - 1 - v : v
      if (!fn[y][x] && i < words.length * 8) { m[y][x] = ((words[i >>> 3] >>> (7 - (i & 7))) & 1) === 1; i++ }
    }
  }

  // The mask with the lowest penalty: runs, 2x2 blocks and balance.
  const masks = [
    (x, y) => (x + y) % 2 === 0, (x, y) => y % 2 === 0, x => x % 3 === 0, (x, y) => (x + y) % 3 === 0,
    (x, y) => (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0, (x, y) => (x * y) % 2 + (x * y) % 3 === 0,
    (x, y) => ((x * y) % 2 + (x * y) % 3) % 2 === 0, (x, y) => ((x + y) % 2 + (x * y) % 3) % 2 === 0,
  ]
  const applyMask = k => {
    for (let y = 0; y < size; y++) for (let x = 0; x < size; x++) if (!fn[y][x] && masks[k](x, y)) m[y][x] = !m[y][x]
  }
  const penalty = () => {
    let p = 0, dark = 0
    for (let a = 0; a < size; a++) {
      for (const get of [b => m[a][b], b => m[b][a]]) {
        let run = 1
        for (let b = 1; b < size; b++) {
          if (get(b) === get(b - 1)) { run++; if (run === 5) p += 3; else if (run > 5) p++ } else run = 1
        }
      }
    }
    for (let y = 0; y < size; y++) for (let x = 0; x < size; x++) {
      if (m[y][x]) dark++
      if (x < size - 1 && y < size - 1 && m[y][x] === m[y][x + 1] && m[y][x] === m[y + 1][x] && m[y][x] === m[y + 1][x + 1]) p += 3
    }
    return p + Math.floor(Math.abs(dark * 20 - size * size * 10) / (size * size)) * 10
  }
  let best = 0, bestP = Infinity
  for (let k = 0; k < 8; k++) {
    applyMask(k); formatBits(k)
    const p = penalty()
    if (p < bestP) { best = k; bestP = p }
    applyMask(k)
  }
  applyMask(best); formatBits(best)
  return m
}

// qrSvg returns SVG markup for text, with a 4-module quiet zone.
export function qrSvg(text, { border = 4, dark = "#000", light = "#fff" } = {}) {
  const m = qrMatrix(text), n = m.length + border * 2
  let d = ""
  m.forEach((row, y) => row.forEach((on, x) => { if (on) d += `M${x + border},${y + border}h1v1h-1z` }))
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${n} ${n}" shape-rendering="crispEdges" role="img" aria-label="QR code">` +
    `<rect width="${n}" height="${n}" fill="${light}"/><path d="${d}" fill="${dark}"/></svg>`
}

// ---------- the panel ----------

const DAY = 24 * 3600 * 1000

function h(tag, props = {}, ...kids) {
  const el = document.createElement(tag)
  for (const [k, v] of Object.entries(props)) {
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v)
    else if (k === "part" || k.startsWith("aria-") || k === "for") el.setAttribute(k, v)
    else el[k] = v
  }
  el.append(...kids.flat().filter(c => c !== null && c !== undefined && c !== false))
  return el
}

const when = ms => ms ? new Date(ms).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" }) : "never"
const ago = ms => {
  if (!ms) return "not yet"
  const d = Math.round((Date.now() - ms) / DAY)
  return d < 1 ? "today" : d === 1 ? "yesterday" : `${d} days ago`
}

const PUBLIC = [
  ["none", "Only people you invite", "Nobody else can see this exists."],
  ["read", "Anyone can look, invited people change it", "Anyone who has the address can read everything here."],
  ["respond", "Anyone can look and add their own answer", "Anyone who has the address can read everything here and add their own entry, like a vote."],
  ["all", "Anyone can look and change it", "Anyone who has the address can read and change everything here."],
]

const CSS = `
:host { display: inline-block; font: inherit; color: inherit; --a: var(--cubby-accent, #2a6df4); --r: var(--cubby-radius, 8px); }
button { font: inherit; border: 1px solid currentColor; border-radius: var(--r); background: none; color: inherit; padding: .35em .8em; cursor: pointer; }
button.main { background: var(--a); border-color: var(--a); color: #fff; }
button:disabled { opacity: .5; cursor: default; }
dialog { font: inherit; color: CanvasText; background: Canvas; border: 1px solid #8884; border-radius: var(--r); padding: 1.2em 1.4em; width: min(30em, 92vw); max-height: 90vh; overflow: auto; }
dialog::backdrop { background: #0006; }
h2 { font-size: 1.15em; margin: 0 0 .6em; }
h3 { font-size: 1em; margin: 1.4em 0 .5em; }
p, li { margin: .3em 0; }
.row { display: flex; gap: .5em; flex-wrap: wrap; align-items: center; margin: .5em 0; }
input[type=text], input[type=number] { font: inherit; padding: .35em .5em; border: 1px solid #8886; border-radius: var(--r); background: none; color: inherit; }
input.link { width: 100%; box-sizing: border-box; font-size: .85em; }
.qr { width: 11em; margin: .5em 0; }
.qr svg { width: 100%; height: auto; display: block; }
.muted { opacity: .7; font-size: .9em; }
ul { padding: 0; list-style: none; }
li { display: flex; justify-content: space-between; gap: .5em; align-items: center; padding: .3em 0; border-top: 1px solid #8883; }
label.choice { display: block; margin: .4em 0; }
.key { font-family: ui-monospace, monospace; font-size: .85em; word-break: break-all; padding: .5em; border: 1px dashed #8888; border-radius: var(--r); }
.close { float: right; border: none; font-size: 1.3em; padding: 0 .3em; }
.shared { opacity: .7; font-size: .9em; }
`

// Outside a browser (a worker, Node) the module still loads, for qrSvg.
const Base = typeof HTMLElement === "undefined" ? class {} : HTMLElement

class CubbyShare extends Base {
  static observedAttributes = ["space", "server", "base", "members", "open"]

  constructor() {
    super()
    this.attachShadow({ mode: "open" })
    this._ownerToken = null
    this._access = null
  }

  set ownerToken(v) {
    this._ownerToken = v
    if (v) this._load().then(() => this._access?.owner && this._show("recovery"))
  }
  get ownerToken() { return this._ownerToken }

  connectedCallback() { this._load() }
  attributeChangedCallback() { if (this.isConnected) this._load() }

  // Call after the page redeemed an invite or changed something itself.
  refresh() { return this._load() }

  get _db() {
    const space = this.getAttribute("space")
    if (!space) return null
    const server = this.getAttribute("server") || undefined
    return kv(space, server ? { server } : {})
  }

  async _load() {
    const db = this._db
    const root = this.shadowRoot
    if (!db) { root.replaceChildren(); return }
    try {
      this._access = await db.access()
    } catch {
      this._access = null
    }
    const a = this._access
    const style = h("style", {}, CSS)
    if (a?.owner) {
      root.replaceChildren(style, h("button", { part: "button", className: "main", onclick: () => this._show("invite") }, "Share"))
      if (this.hasAttribute("open")) this._show("invite")
    } else if (a?.inside && a.grants.length && this.hasAttribute("members")) {
      root.replaceChildren(style, h("span", { className: "shared", part: "button" }, "Shared with you by the owner"))
    } else {
      root.replaceChildren(style)
    }
  }

  _changed() { this.dispatchEvent(new CustomEvent("cubby-change", { bubbles: true, composed: true })) }

  async _show(focus) {
    const db = this._db
    this.shadowRoot.querySelector("dialog")?.remove()
    const dlg = h("dialog", { part: "panel", "aria-label": "Share" })
    dlg.addEventListener("close", () => dlg.remove())
    this.shadowRoot.append(dlg)
    const render = async () => {
      const [tokens, grants] = await Promise.all([db.owner.tokens.list(), db.owner.grants.list()])
      const settings = (await db.access()).settings
      dlg.replaceChildren(
        h("button", { className: "close", "aria-label": "Close", onclick: () => dlg.close() }, "×"),
        h("h2", {}, "Share"),
        this._recovery(db, focus === "recovery"),
        this._invite(db, render),
        this._people(db, tokens, grants, render),
        this._visibility(db, settings, render),
      )
    }
    await render()
    dlg.showModal()
  }

  _recovery(db, now) {
    const box = h("section")
    const showKey = (key, note) => box.replaceChildren(
      h("h3", {}, "Recovery key"),
      h("p", {}, note),
      h("p", { className: "key" }, key),
      h("div", { className: "row" },
        h("button", { onclick: e => { navigator.clipboard?.writeText(key); e.target.textContent = "Copied" } }, "Copy"),
      ),
    )
    if (now && this._ownerToken) {
      showKey(this._ownerToken, "Save this in a password manager. It's the only way back in if you clear this browser or use another device, and it's shown only now.")
      this._ownerToken = null
      return box
    }
    return box
  }

  _invite(db, rerender) {
    const name = h("input", { type: "text", placeholder: "Who is it for?", "aria-label": "Who is it for?" })
    const result = h("div")
    const more = h("details", {},
      h("summary", { className: "muted" }, "More options"),
    )
    const uses = h("input", { type: "number", min: 1, value: 1, style: "width:5em", "aria-label": "Uses" })
    const days = h("input", { type: "number", min: 1, value: 7, style: "width:5em", "aria-label": "Days" })
    let armed = false
    const forever = h("button", {
      onclick: async e => {
        if (!armed) {
          armed = true
          e.target.textContent = "Click again: anyone with this link can join, with no limit, until you cancel it"
          return
        }
        await make({ uses: null, until: null })
      },
    }, "Link with no limits")
    more.append(
      h("div", { className: "row" }, "Uses", uses, "Valid for", days, "days",
        h("button", { onclick: () => make({ uses: Number(uses.value) || 1, until: `${Number(days.value) || 7}d` }) }, "Create")),
      h("div", { className: "row" }, forever),
    )
    const make = async o => {
      const label = name.value.trim()
      const inv = await db.share({ label, ...o, base: this.getAttribute("base") || undefined })
      this.dispatchEvent(new CustomEvent("cubby-invite", { detail: inv, bubbles: true, composed: true }))
      const link = h("input", { type: "text", readOnly: true, className: "link", value: inv.link })
      const qr = h("div", { className: "qr" })
      qr.innerHTML = qrSvg(inv.link)
      const limits = inv.uses === null ? "No limit on uses or time." :
        `Works for ${inv.uses === 1 ? "one person" : `${inv.uses} people`} until ${when(inv.until)}.`
      result.replaceChildren(
        h("p", {}, label ? `Invite for ${label}. ` : "", limits),
        link,
        h("div", { className: "row" },
          h("button", { onclick: e => { navigator.clipboard?.writeText(inv.link); e.target.textContent = "Copied" } }, "Copy"),
          navigator.share ? h("button", { onclick: () => navigator.share({ url: inv.link, title: "Invite" }).catch(() => {}) }, "Share") : null,
        ),
        qr,
      )
      this._changed()
    }
    return h("section", {},
      h("h3", {}, "Invite"),
      h("div", { className: "row" }, name),
      h("div", { className: "row" },
        h("button", { className: "main", onclick: () => make({ uses: 1 }) }, "One person"),
        h("button", { onclick: () => make({}) }, "A group link"),
        h("button", { onclick: () => make({ uses: 1, allow: "viewer" }) }, "View only"),
      ),
      h("p", { className: "muted" }, "One person: one use, 7 days. A group link: 10 people, 7 days."),
      more,
      result,
    )
  }

  _people(db, tokens, grants, rerender) {
    const labels = new Map(tokens.map(t => [t.id, t.label]))
    const byUser = new Map()
    for (const g of grants) {
      if (g.owner) continue
      const p = byUser.get(g.user) || { user: g.user, labels: [], created: g.createdAt, last: g.lastUsedAt }
      p.labels.push(labels.get(g.source) || (g.source === "direct" ? "added directly" : "invite"))
      p.last = Math.max(p.last || 0, g.lastUsedAt || 0)
      byUser.set(g.user, p)
    }
    const people = [...byUser.values()]
    const pending = tokens.filter(t => t.state === "active")
    const act = fn => async () => { await fn(); this._changed(); rerender() }
    return h("section", {},
      h("h3", {}, "People"),
      people.length ? h("ul", {}, people.map(p => h("li", {},
        h("span", {}, p.labels.filter(Boolean).join(", ") || p.user, h("span", { className: "muted" }, ` · joined ${ago(p.created)}, active ${ago(p.last)}`)),
        h("button", { onclick: act(() => db.owner.grants.revokeUser(p.user)) }, "Remove"),
      ))) : h("p", { className: "muted" }, "Nobody has joined yet."),
      pending.length ? h("div", {},
        h("p", { className: "muted" }, "Open invites"),
        h("ul", {}, pending.map(t => h("li", {},
          h("span", {}, t.label || "Invite", h("span", { className: "muted" },
            ` · ${t.usesLeft === null ? "no limit" : `${t.usesLeft} left`}, until ${when(t.until)}`)),
          h("span", { className: "row" },
            h("button", { onclick: act(() => db.owner.tokens.update(t.id, { until: "7d" })) }, "Extend"),
            h("button", { onclick: act(() => t.grants ? db.owner.tokens.update(t.id, { uses: 0 }) : db.owner.tokens.revoke(t.id)) }, "Cancel"),
          ),
        ))),
      ) : null,
    )
  }

  _visibility(db, settings, rerender) {
    const current = settings?.public
    return h("section", {},
      h("h3", {}, "Who can see this"),
      current === "custom" ? h("p", { className: "muted" }, "Custom settings, set elsewhere.") : null,
      PUBLIC.map(([value, title, warning]) => h("label", { className: "choice" },
        h("input", {
          type: "radio", name: "public", checked: value === current,
          onchange: async e => {
            if (value !== "none" && !confirm(warning + " Continue?")) { e.target.checked = false; rerender(); return }
            await db.owner.settings({ public: value })
            this._changed()
            rerender()
          },
        }), " ", title)),
      h("h3", {}, "Recovery key"),
      h("p", { className: "muted" }, "The recovery key makes any device an owner. Making a new one stops the old one working."),
      h("div", { className: "row" }, h("button", {
        onclick: async e => {
          if (!confirm("Make a new recovery key? The old one stops working, and devices that became owners with it lose that.")) return
          const key = await db.owner.rotateOwnerToken()
          e.target.replaceWith(h("p", { className: "key" }, key), h("p", { className: "muted" }, "Save it now. It isn't shown again."))
        },
      }, "Make a new recovery key")),
    )
  }
}

if (typeof customElements !== "undefined" && !customElements.get("cubby-share")) {
  customElements.define("cubby-share", CubbyShare)
}
