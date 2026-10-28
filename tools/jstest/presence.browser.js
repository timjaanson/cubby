// Presence in a real browser: closing a tab makes it leave through its last
// will, not the timeout. Run against a fresh local server on 28991:
// playwright-cli -s=live-chrome open && playwright-cli -s=live-chrome run-code --filename=tools/jstest/presence.browser.js
async page => {
  const base = 'http://localhost:28991'
  // Pages on the server's own origin, so Chrome allows sockets to localhost.
  await page.goto(base + '/health')
  const space = await page.evaluate(async base => {
    const { createSpace, kv } = await import(base + '/kv.js')
    const { space } = await createSpace({ server: base, public: 'all' })
    const db = kv(space, { server: base, live: true })
    window.here = db.users.presence('here', { value: { name: 'first tab' } })
    window.left = []
    window.here.onLeave = id => window.left.push({ id, at: performance.now() })
    return space
  }, base)
  const tab = await page.context().newPage()
  await tab.goto(base + '/health')
  const other = await tab.evaluate(async ([base, space]) => {
    const { kv } = await import(base + '/kv.js')
    const db = kv(space, { server: base, live: true, tokenKey: 'second-tab-' + Date.now() })
    window.here = db.users.presence('here', { value: { name: 'second tab' } })
    return db.id()
  }, [base, space])
  await page.waitForFunction(id => window.here.peers.get(id)?.value.name === 'second tab', other)
  const closedAt = await page.evaluate(() => performance.now())
  await tab.close()
  await page.waitForFunction(id => window.left.some(l => l.id === id), other, { timeout: 3000 })
  const ms = await page.evaluate(t => Math.round(window.left[0].at - t), closedAt)
  await page.evaluate(() => window.here.stop())
  return { leftAfterTabCloseMs: ms }
}
