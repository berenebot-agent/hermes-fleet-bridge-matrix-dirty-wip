// Opt-in browser test using an already installed Playwright; no runtime dependency.
const fs = require('node:fs/promises');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');

(async () => {
  const repo = path.resolve(__dirname, '..');
  const fixture = JSON.parse(await fs.readFile(path.join(repo, 'tests/logs/browser-fixture.json'), 'utf8'));
  const env = await fs.readFile(path.join(fixture.root, 'bridge.env'), 'utf8');
  const password = env.split('\n').find(line => line.startsWith('ADMIN_PASSWORD=')).slice('ADMIN_PASSWORD='.length);
  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    // Browser-local reverse proxy: real assets/APIs, HTTPS origin, no DNS/NPM changes.
    await context.route('https://chat.test/**', async route => {
      try {
        const url = new URL(route.request().url());
        const response = await route.fetch({ url: fixture.base + url.pathname + url.search });
        await route.fulfill({ response });
      } catch {
        // Closing long-poll syncs must not print request headers/access tokens.
        try { await route.abort(); } catch {}
      }
    });
    await page.goto('https://chat.test/', { waitUntil: 'networkidle' });
    await page.waitForTimeout(1500);
    let text = await page.locator('body').innerText();
    const inputInfo = await page.locator('input').evaluateAll(nodes => nodes.map(n => ({ type: n.type, name: n.name, placeholder: n.placeholder, aria: n.getAttribute('aria-label') })));
    await fs.writeFile(path.join(fixture.root, 'browser-before.json'), JSON.stringify({ text, inputs: inputInfo }, null, 2), { mode: 0o600 });
    const username = page.locator('input[type="text"]').first();
    if (await page.locator('input[type="password"]').count() === 0) {
      const signIn = page.getByRole('link', { name: /sign in/i });
      if (await signIn.count()) await signIn.first().click();
      else await page.getByRole('button', { name: /sign in/i }).first().click();
    }
    await username.fill('admin');
    await page.locator('input[type="password"]').fill(password);
    await page.getByRole('button', { name: /^sign in$/i }).click();
    await page.waitForTimeout(7000);
    text = await page.locator('body').innerText();
    const serviceWorkerWarning=page.getByRole('button',{name:'OK',exact:true});
    if (/Failed to load service worker/.test(text) && await serviceWorkerWarning.count()) await serviceWorkerWarning.click();
    await page.getByText('iris',{exact:true}).first().click();
    await page.waitForTimeout(3000);
    text=await page.locator('body').innerText();
    await fs.writeFile(path.join(fixture.root,'browser-room.json'),JSON.stringify({text},null,2),{mode:0o600});
    for(let attempt=0;attempt<3 && await page.getByText('2 replies',{exact:true}).count()===0;attempt++) {
      await page.locator('.mx_EventTile').first().evaluate(el=>{
        for(let parent=el;parent;parent=parent.parentElement) {
          if(parent.clientHeight>100 && parent.scrollHeight>parent.clientHeight+100){parent.scrollTop=0;return;}
        }
      });
      await page.waitForTimeout(1500);
    }
    await page.getByText('2 replies',{exact:true}).first().click();
    await page.getByText('thread-a-check',{exact:true}).waitFor({state:'visible',timeout:10000});
    text=await page.locator('body').innerText();
    await fs.writeFile(path.join(fixture.root, 'browser-after.json'), JSON.stringify({ text }, null, 2), { mode: 0o600 });
    await page.screenshot({ path: path.join(fixture.root, 'browser.png') });
    const roomVisible = /iris/.test(text);
    const blockedWizard = /verify this device|set up recovery.*continue|verify your identity/i.test(text);
    if (!roomVisible || blockedWizard || !text.includes('Integration reply')) throw new Error('Room/history missing or a blocking verification wizard appeared; see private browser artifacts');
    console.log('PASS actual Element browser: username/password login, provisioned room, reply history and thread conversation accessible, no required device verification');
    await fs.writeFile(path.join(fixture.root, 'browser-result.json'), JSON.stringify({ passed: true, roomVisible, blockedWizard }), { mode: 0o600 });
    await context.unrouteAll({ behavior: 'ignoreErrors' });
  } finally {
    await browser.close();
  }
})().catch(() => {
  // Never print an automation exception that could include a password/input value.
  console.error('FAIL browser acceptance; inspect private tests/logs integration artifacts');
  process.exitCode = 1;
});
