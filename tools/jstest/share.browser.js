// Run against a fresh local server on 28991:
// playwright-cli -s=cubby-check run-code --filename=tools/jstest/share.browser.js
async page => {
  const base = 'http://localhost:28991'
  await page.route(base + '/browser-check', route => route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>Cubby browser checks</title><body></body>' }))
  await page.goto(base + '/browser-check')
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  await page.evaluate(async base => {
    const { createSpace, kv } = await import(base + '/kv.js')
    await import(base + '/share.js')
    const { space, ownerToken } = await createSpace({ server: base })
    window.testDB = kv(space, { server: base })
    window.testRecovery = ownerToken
    const el = document.createElement('cubby-share')
    el.setAttribute('server', base)
    el.setAttribute('space', space)
    document.body.append(el)
    // The original failing order. Setting open also starts an async load.
    el.ownerToken = ownerToken
    el.setAttribute('open', '')
  }, base)
  await page.locator('cubby-share .key').waitFor()
  if (!await page.evaluate(() => document.querySelector('cubby-share').shadowRoot.querySelector('.key')?.textContent === window.testRecovery)) throw Error('Recovery key lost after ownerToken then open')
  // Reload while the dialog is open. Recovery remains until dismissed.
  await page.evaluate(() => document.querySelector('cubby-share').refresh())
  await page.locator('cubby-share .key').waitFor()
  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('button', { name: 'Share', exact: true }).click()
  await page.getByRole('button', { name: 'Create group link', exact: true }).waitFor()
  if (await page.locator('cubby-share .key').count()) throw Error('Recovery key shown after dismissal')
  await page.getByRole('button', { name: 'Create group link', exact: true }).click()
  await page.getByRole('button', { name: 'Copy', exact: true }).waitFor()
  // Reverse assignment order also keeps the key visible.
  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await page.evaluate(() => {
    const el = document.querySelector('cubby-share')
    el.setAttribute('open', '')
    el.ownerToken = window.testRecovery
  })
  await page.locator('cubby-share .key').waitFor()
  const clock = await page.evaluate(async () => {
    const timing = await window.testDB.clock.sync({ samples: 2 })
    const now = window.testDB.clock.now()
    window.testDB.clock.dispose()
    return { rtt: timing.rtt, error: Math.abs(now - Date.now()) }
  })
  if (clock.error > 1000) throw Error('Browser clock offset incorrect')
  if (errors.length) throw Error(errors.join('\n'))
  return { recoveryOrdering: 'passed both orders', recoveryDismissal: 'passed', inviteCreation: 'passed', clock: 'passed' }
}
