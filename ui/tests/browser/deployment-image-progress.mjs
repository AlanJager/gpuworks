import assert from 'node:assert/strict';
import { mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import { resolve, dirname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { gzipSync } from 'node:zlib';
import { build } from 'esbuild';
import { chromium } from 'playwright-core';

const ui = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const out = resolve(process.argv[2] ?? '/tmp/deployment-image-progress-report');
await mkdir(out, { recursive: true });
const result = await build({ entryPoints: [resolve(ui, 'tests/fixtures/deploymentImageProgress.tsx')], bundle: true,
  platform: 'browser', format: 'iife', jsx: 'automatic', minify: true, write: false, target: 'es2022',
  alias: { '@': resolve(ui, 'src') }, define: { 'process.env.NODE_ENV': '"production"', 'import.meta.env': 'undefined' } });
const assetsDir = resolve(ui, '../internal/server/webui/dist/assets');
const css = await readFile(resolve(assetsDir, (await readdir(assetsDir)).find(name => /^index-.*\.css$/.test(name))), 'utf8');
const payload = gzipSync(JSON.stringify({ css, js: result.outputFiles[0].text })).toString('base64');
const html = `<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; font-src data:; connect-src 'none'">
<title>GWKS-407 · 镜像准备进度预览</title></head><body><div id="root"></div><script>
(async()=>{const a=JSON.parse(await new Response(new Blob([Uint8Array.from(atob('${payload}'),c=>c.charCodeAt(0))]).stream().pipeThrough(new DecompressionStream('gzip'))).text());
const style=document.createElement('style');style.textContent=a.css;document.head.append(style);const script=document.createElement('script');script.textContent=a.js;document.body.append(script)})();</script></body></html>`;
const preview = resolve(out, '镜像准备进度预览.html');
await writeFile(preview, html);
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const results = [];
try {
  for (const lang of ['zh', 'en']) for (const width of [1440, 1024]) {
    const page = await browser.newPage({ viewport: { width, height: 1100 } });
    page.setDefaultTimeout(10000);
    const errors = [];
    page.on('pageerror', error => { errors.push(error.message); console.error(error.message); });
    await page.goto(`${pathToFileURL(preview)}#lang=${lang}`);
    const region = page.getByLabel(lang === 'zh' ? '镜像准备进度' : 'Image preparation progress', { exact: true });
    for (const scene of ['syncing', 'pulling', 'unknown', 'missing', 'zero', 'finishing', 'present', 'failed', 'canceled', 'unavailable', 'waiting', 'ambiguous']) {
      await page.getByLabel('模拟状态').selectOption(scene);
      await region.waitFor();
      const stateText = { syncing: ['同步镜像', 'Syncing image'], missing: ['同步镜像', 'Syncing image'], pulling: ['下载镜像', 'Downloading image'], unknown: ['下载镜像', 'Downloading image'],
        zero: ['下载镜像', 'Downloading image'], finishing: ['下载镜像', 'Downloading image'], present: ['镜像已就绪', 'Image ready'],
        failed: ['镜像传输失败', 'Image transfer failed'], canceled: ['镜像传输已取消', 'Image transfer canceled'], unavailable: ['暂时无法获取进度', 'Progress unavailable'],
        waiting: ['等待进度上报', 'Waiting for progress'], ambiguous: ['等待进度上报', 'Waiting for progress'] }[scene][lang === 'zh' ? 0 : 1];
      await region.getByText(stateText, { exact: true }).waitFor();
      await region.scrollIntoViewIfNeeded();
      const measured = ['syncing', 'pulling', 'zero', 'finishing'].includes(scene);
      const indeterminate = ['unknown', 'missing'].includes(scene);
      assert.equal(await region.getByRole('progressbar').count(), measured || indeterminate ? 1 : 0, scene);
      if (indeterminate) {
        const bar = region.getByRole('progressbar');
        assert.equal(await bar.getAttribute('aria-valuenow'), null);
        const animation = await bar.evaluate(e => {
          const indicator = e.firstElementChild;
          return { name: getComputedStyle(indicator).animationName, iterations: getComputedStyle(indicator).animationIterationCount,
            running: indicator.getAnimations().some(a => a.playState === 'running'), width: e.getBoundingClientRect().width };
        });
        assert.equal(animation.name, 'instance-progress-slide');
        assert.equal(animation.iterations, 'infinite');
        assert.ok(animation.running && animation.width > 100);
        if (scene === 'missing') assert.ok(!(await region.innerText()).includes('0 B'));
      }
      assert.equal(await page.getByText(/Check its transfer progress on the images page|可在镜像页查看传输进度/).count(), 0);
      if (measured) assert.equal(await region.getByRole('progressbar').getAttribute('aria-valuenow'), scene === 'zero' ? '0' : scene === 'finishing' ? '100' : '37');
      if (['present', 'failed', 'canceled', 'finishing'].includes(scene)) assert.ok(!(await region.innerText()).includes('/s'), scene);
      assert.ok(await region.evaluate(element => element.scrollWidth <= element.clientWidth + 1), `${scene}: region overflow`);
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${scene}: page overflow`);
      const resources = page.getByRole('heading', { name: lang === 'zh' ? '资源准备' : 'Resource preparation', exact: true });
      assert.equal(await resources.count(), 1);
      assert.ok(await region.evaluate(element => element.closest('.detail-section')?.querySelectorAll('[role="progressbar"]').length >= 1));
      const columns = await page.locator('.deployment-overview-columns').evaluate(element => getComputedStyle(element).gridTemplateColumns);
      assert.equal(columns.split(' ').length, 1, 'the approved single-column layout is preserved');
      await page.screenshot({ path: resolve(out, `${lang}-${width}-${scene}.png`), fullPage: true });
      if (scene === 'unavailable') {
        await region.getByRole('button', { name: lang === 'zh' ? '重新获取' : 'Reload' }).click();
        await region.getByRole('progressbar').waitFor();
        await page.screenshot({ path: resolve(out, `${lang}-${width}-recovered.png`), fullPage: true });
      }
      results.push({ lang, width, scene, passed: true });
    }
    await page.getByLabel('模拟状态').selectOption('syncing');
    await region.getByRole('progressbar').waitFor();
    await page.evaluate(() => window.setTransferredBytes(4 * 1024 ** 3));
    await page.waitForFunction(() => document.querySelector('[role="progressbar"][aria-label*="gpu-target"]')?.getAttribute('aria-valuenow') === '50');
    await page.screenshot({ path: resolve(out, `${lang}-${width}-polled.png`), fullPage: true });
    assert.deepEqual(errors, []);
    await page.close();
  }
  for (const lang of ['zh','en']) for (const width of [1440,1024]) {
    const page=await browser.newPage({viewport:{width,height:1100}});page.setDefaultTimeout(10000);
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.goto(`${pathToFileURL(preview)}#lang=${lang}&resources=1`);
    const heading=page.getByRole('heading',{name:lang==='zh'?'资源准备':'Resource preparation',exact:true});
    await heading.waitFor();
    const region=page.getByLabel(lang==='zh'?'镜像准备进度':'Image preparation progress',{exact:true});
    for(const step of ['model-only','model-ready','parallel','model-done','image-done','ready','starting']) {
      await page.evaluate(step=>window.advanceResourceStep(step),step);
      if(step==='starting') { await heading.waitFor({state:'hidden'}); }
      else {
        await heading.waitFor();await heading.scrollIntoViewIfNeeded();
        const hasImage=!['model-only','model-ready'].includes(step);
        assert.equal(await region.count(),hasImage?1:0,step+' image visibility');
        if(hasImage)await region.getByText(['image-done','ready'].includes(step)?(lang==='zh'?'镜像已就绪':'Image ready'):(lang==='zh'?'同步镜像':'Syncing image'),{exact:true}).waitFor();
        const section=heading.locator('xpath=ancestor::section[1]');
        const expectedBars=step==='parallel'?2:['model-ready','ready'].includes(step)?0:1;
        assert.equal(await section.getByRole('progressbar').count(),expectedBars,step+' bars');
        assert.equal(await section.locator('[data-status="done"]').count(),step==='ready'?2:['model-ready','model-done','image-done'].includes(step)?1:0,step+' checkmarks');
        if(['model-ready','ready'].includes(step))assert.ok(!(await section.innerText()).includes('/s'));
      }
      await page.screenshot({path:resolve(out,`retention-${lang}-${width}-${step}.png`),fullPage:true});
      results.push({lang,width,scene:step,passed:true});
    }
    assert.deepEqual(errors,[]);await page.close();
  }
  await writeFile(resolve(out, 'results.json'), JSON.stringify(results, null, 2));
  console.log(JSON.stringify({ preview, states: results.length, screenshots: (await readdir(out)).filter(name => name.endsWith('.png')).length }));
} finally { await browser.close(); }
