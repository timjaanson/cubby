import jsQR from "jsqr"
const { qrMatrix } = await import(process.env.MOD)
let ok = 0
const texts = ["hi", "https://example.com/list/#list=family-cedar-pilot-mango-frost&t=at_" + "A".repeat(43)]
for (const n of [1, 10, 17, 30, 60, 100, 150, 200, 250, 300, 400, 600, 900, 1500, 2000])
  texts.push(Array.from({ length: n }, (_, i) => "abcXYZ-_.~0123456789:/#&=?é"[(i * 7) % 27]).join(""))
for (const t of texts) {
  const m = qrMatrix(t)
  const scale = 4, border = 4, n = (m.length + border * 2) * scale
  const data = new Uint8ClampedArray(n * n * 4).fill(255)
  for (let y = 0; y < m.length; y++) for (let x = 0; x < m.length; x++) if (m[y][x])
    for (let dy = 0; dy < scale; dy++) for (let dx = 0; dx < scale; dx++) {
      const i = (((y + border) * scale + dy) * n + (x + border) * scale + dx) * 4
      data[i] = data[i + 1] = data[i + 2] = 0
    }
  const r = jsQR(data, n, n)
  if (!r || r.data !== t) { console.log("FAIL len", t.length, "version", (m.length - 17) / 4, r ? JSON.stringify(r.data.slice(0, 40)) : null); continue }
  ok++
}
console.log(ok, "of", texts.length, "decoded")
if (ok !== texts.length) process.exit(1)
