// kv.js with live: true in a real browser engine. Run against a fresh local
// server on 28991, in Chromium and in WebKit (Safari's engine):
// playwright-cli -s=live-chrome open && playwright-cli -s=live-chrome run-code --filename=tools/jstest/live.browser.js
// playwright-cli -s=live-webkit open --browser=webkit && playwright-cli -s=live-webkit run-code --filename=tools/jstest/live.browser.js
async page => {
  const base = 'http://localhost:28991'
  const frames = { sent: 0, received: 0 }
  page.on('websocket', ws => {
    ws.on('framesent', () => frames.sent++)
    ws.on('framereceived', () => frames.received++)
  })
  // A page on the server's own origin. An intercepted page counts as public
  // to Chrome, which then blocks sockets to localhost (Local Network Access).
  await page.goto(base + '/health')
  const out = await page.evaluate(async base => {
    const sleep = ms => new Promise(r => setTimeout(r, ms))
    const until = async (f, m, ms = 5000) => {
      const end = Date.now() + ms
      while (Date.now() < end) { if (await f()) return; await sleep(10) }
      throw Error('timeout: ' + m)
    }
    const { createSpace, kv } = await import(base + '/kv.js')
    const { space } = await createSpace({ server: base, public: 'all' })
    const a = kv(space, { server: base, live: true })
    const b = kv(space, { server: base, tokenKey: 'second-person' }) // plain HTTP participant
    const states = []
    a.live.onStatus = s => states.push(s)

    const got = []
    let synced = false
    const stop = a.shared.watch('card.*', c => got.push(c), { onSync: () => { synced = true } })
    await until(() => synced && a.live.status === 'open', 'live watch synced')

    // The HTTP participant writes; the live one sees it pushed.
    const t0 = performance.now()
    await b.shared.set('card.1', { x: 10 })
    await until(() => got.some(c => c.key === 'card.1'), 'push from http writer')
    const pushMs = performance.now() - t0

    // The live participant writes over the socket; the HTTP one reads it.
    const t1 = performance.now()
    for (let i = 0; i < 20; i++) await a.shared.set('card.2', { x: i })
    const writeMs = (performance.now() - t1) / 20
    const seen = await b.shared.getEntry('card.2')
    if (seen.value.x !== 19) throw Error('http read of live write')
    const e = await a.shared.getEntry('card.2')
    if (!e.mine || !e.t || e.value.x !== 19) throw Error('live getEntry ' + JSON.stringify(e))

    // pagehide closes the socket (back/forward cache); pageshow brings it back.
    const mark = states.length
    window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }))
    await until(() => states.slice(mark).includes('closed'), 'closed on pagehide')
    window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }))
    await until(() => a.live.status === 'open', 'reopened on pageshow')
    await b.shared.set('card.3', 'after')
    await until(() => got.some(c => c.key === 'card.3'), 'watch resumed after pageshow')
    stop()

    // Pulses: a live listener and an HTTP listener, 30 quick pulses each way.
    const onA = [], onB = []
    const stopA = a.users.onPulse('cursor', p => { if (!p.mine) onA.push(p) })
    const stopB = b.users.onPulse('cursor', p => { if (!p.mine) onB.push(p) })
    await sleep(200)
    const t2 = performance.now()
    // maxRate 0: every one of a burst must arrive, past the client throttle.
    for (let i = 0; i < 30; i++) { a.user().pulse('cursor', { x: i }, { maxRate: 0 }); b.user().pulse('cursor', { x: i }, { maxRate: 0 }) }
    await until(() => onA.length === 30 && onB.length === 30, `pulses ${onA.length}/${onB.length}`)
    const pulseMs = performance.now() - t2
    if (await a.shared.get('cursor') !== null) throw Error('pulse was stored')
    stopA(); stopB()
    return { pushMs: Math.round(pushMs), writeMs: Math.round(writeMs * 10) / 10, pulse60Ms: Math.round(pulseMs), states }
  }, base)
  if (frames.received < 20) throw Error('few socket frames: ' + JSON.stringify(frames))
  return { ...out, frames, ua: await page.evaluate(() => navigator.userAgent) }
}
