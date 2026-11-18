// Run against a fresh local server on 28991:
// playwright-cli -s=cubby-check run-code --filename=tools/jstest/share.browser.js
async page => {
  const base = 'http://localhost:28991'
  // A page on the server's own origin (Chrome's Local Network Access).
  await page.goto(base + '/health')
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  await page.evaluate(async base => {
    document.documentElement.innerHTML = '<body></body>'
    const { visitor, kv } = await import(base + '/kv.js')
    await import(base + '/share.js')
    const { space } = await visitor({ server: base }).createSpace()
    window.testDB = kv(space, { server: base })
    window.testSpace = space
    const el = document.createElement('cubby-share')
    el.setAttribute('server', base)
    el.setAttribute('space', space)
    document.body.append(el)
    el.showBackup()
  }, base)
  // The backup step comes first after createSpace.
  await page.getByRole('button', { name: 'Save a backup key', exact: true }).click()
  await page.locator('cubby-share textarea.key').waitFor()
  const backup = await page.evaluate(() => document.querySelector('cubby-share').shadowRoot.querySelector('textarea.key').value)
  const parsed = JSON.parse(backup)
  if (!parsed.key?.d || !parsed.visitor?.startsWith('u_')) throw Error('backup text ' + backup)
  // Closed and reopened, the panel starts at invites, the key is gone.
  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('button', { name: 'Share', exact: true }).click()
  await page.getByRole('button', { name: 'Create link', exact: true }).waitFor()
  if (await page.locator('cubby-share textarea.key').count()) throw Error('backup key shown after dismissal')
  await page.getByRole('button', { name: 'Create link', exact: true }).click()
  await page.getByRole('button', { name: 'Copy', exact: true }).waitFor()
  const groupInv = await page.evaluate(async () => (await window.testDB.invites.list()).map(i => [i.access, i.uses]))
  if (JSON.stringify(groupInv) !== '[["all",null]]') throw Error('default link ' + JSON.stringify(groupInv))
  // An owner link: a level under More options, one use.
  await page.locator('cubby-share summary').click()
  await page.getByRole('combobox', { name: 'Level' }).selectOption('owner')
  await page.getByRole('button', { name: 'Create link', exact: true }).click()
  await page.waitForFunction(() => /Owner, one person/.test(document.querySelector('cubby-share').shadowRoot.textContent))
  const ownerInv = await page.evaluate(async () => {
    const link = document.querySelector('cubby-share').shadowRoot.querySelector('input.link').value
    const list = await window.testDB.invites.list()
    return { link, owner: list.filter(i => i.access === 'owner').map(i => i.uses) }
  })
  if (!ownerInv.link.includes('#space=' ) || !ownerInv.link.includes('&invite=inv_') || ownerInv.owner.join() !== '1') throw Error('owner link ' + JSON.stringify(ownerInv))
  await page.getByRole('button', { name: 'Close', exact: true }).click()
  // The backup restores the owner under another key name, as on a new device.
  const restored = await page.evaluate(async ([base, backup]) => {
    const { visitor, kv } = await import(base + '/kv.js')
    const keyName = 'restored-' + Date.now()
    const r = await visitor({ server: base, keyName }).restore(backup)
    const me = await kv(window.testSpace, { server: base, keyName }).open({ invite: null })
    const owner = await window.testDB.open({ invite: null })
    return { id: r.id, access: me.access, ownerId: owner.id }
  }, [base, backup])
  if (restored.id !== restored.ownerId || restored.access !== 'owner') throw Error('restore ' + JSON.stringify(restored))
  const clock = await page.evaluate(async () => {
    const timing = await window.testDB.clock.sync({ samples: 2 })
    const now = window.testDB.clock.now()
    window.testDB.clock.dispose()
    return { rtt: timing.rtt, error: Math.abs(now - Date.now()) }
  })
  if (clock.error > 1000) throw Error('Browser clock offset incorrect')
  // A member gets the panel too: invites and their own open links, nothing
  // an owner manages. The page's visitor joins someone else's space.
  const space2 = await page.evaluate(async base => {
    const { visitor, kv } = await import(base + '/kv.js')
    const opt = { server: base, keyName: 'other-owner-' + Date.now() }
    const { space } = await visitor(opt).createSpace()
    const { invite } = await kv(space, opt).invites.create({ uses: 1 })
    const me = await kv(space, { server: base }).open({ invite })
    if (!me.canInvite) throw Error('member canInvite')
    return space
  }, base)
  await page.evaluate(space => {
    const el = document.querySelector('cubby-share')
    el.setAttribute('space', space)
  }, space2)
  await page.getByRole('button', { name: 'Share', exact: true }).click()
  await page.getByRole('button', { name: 'Create link', exact: true }).click()
  await page.getByRole('button', { name: 'Copy', exact: true }).waitFor()
  const memberPanel = await page.evaluate(() => {
    const root = document.querySelector('cubby-share').shadowRoot
    const heads = [...root.querySelectorAll('h3')].map(h => h.textContent)
    return { heads, link: root.querySelector('input.link')?.value || '' }
  })
  if (await page.locator('cubby-share option[value=owner]').count()) throw Error('member sees the owner level')
  if (memberPanel.heads.includes('People') || memberPanel.heads.includes('Who can see this') || !memberPanel.link.includes('&invite=inv_')) throw Error('member panel ' + JSON.stringify(memberPanel))
  // The key survives a reload: same visitor from IndexedDB.
  const before = await page.evaluate(async base => (await import(base + '/kv.js')).visitor({ server: base }).devices.list().then(l => l.length), base)
  const id1 = await page.evaluate(async base => { const { kv } = await import(base + '/kv.js'); return (await kv(window.testSpace, { server: base }).open({ invite: null })).id }, base)
  await page.reload()
  const id2 = await page.evaluate(async ([base, space]) => { const { kv } = await import(base + '/kv.js'); return (await kv(space, { server: base }).open({ invite: null })).id }, [base, space2])
  if (!id1 || id1 !== id2 || before < 1) throw Error('identity after reload ' + id1 + ' ' + id2)
  if (errors.length) throw Error(errors.join('\n'))
  return { backupFirst: 'passed', dismissal: 'passed', ownerLink: 'passed', restore: 'passed', clock: 'passed', memberPanel: 'passed', reload: 'passed' }
}
