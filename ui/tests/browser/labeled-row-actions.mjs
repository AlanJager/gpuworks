// Layout check of a group header's labelled actions (RowActions `labeled`, as
// the E-HPC cluster group header carries them; a table row keeps icon buttons), with
// synthetic data: each button is one horizontal row, its icon left of its name,
// as tall as a small control, and its text is not cut. Not a live server test.
// Usage: node tests/browser/labeled-row-actions.mjs /absolute/output/path
import assert from 'node:assert/strict';
import { mkdir, writeFile, unlink } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { createServer } from 'vite';
import { chromium } from 'playwright-core';
const root = fileURLToPath(new URL('../../', import.meta.url));
const out = resolve(process.argv[2] ?? '/tmp/gpuworks-labeled-row-actions');
await mkdir(out, { recursive: true });
const fixture = resolve(root, '.__labeled-row-actions.html');
const html = `<!doctype html><html><head><meta charset="utf-8"><script>
const p=new URLSearchParams(location.search);document.documentElement.dataset.theme=p.get('theme');document.documentElement.classList.toggle('dark',p.get('theme')==='dark');
</script></head><body><div id="root"></div><script type="module">
import '/src/app.css';import '/src/theme/layers.css';
import React from 'react';import {createRoot} from 'react-dom/client';import {MemoryRouter} from 'react-router-dom';
import {createInstance} from 'i18next';import {I18nextProvider,initReactI18next} from 'react-i18next';
import {TableGroup} from '/src/pages/platforms/GroupRows.tsx';import SlurmEntryActions from '/src/pages/slurm/SlurmEntryActions.tsx';import SlurmEntryStatus from '/src/pages/slurm/SlurmEntryStatus.tsx';
import {Table} from '/src/components/ui/table';
const bundles=import.meta.glob('/src/i18n/locales/*/*.json',{eager:true,import:'default'});const resources={};for(const [path,data] of Object.entries(bundles)){const parts=path.split('/');(resources[parts.at(-2)]??={})[parts.at(-1).replace('.json','')]=data;}
const locale=new URLSearchParams(location.search).get('locale');const i18n=createInstance();await i18n.use(initReactI18next).init({lng:locale,resources,defaultNS:'common',interpolation:{escapeValue:false}});
const e=React.createElement;
const entry={id:7,uuid:'s-7',name:'slurm25.05.5-cluster-20261118',server_url:'https://gpuworks.example',state:'pending',stalled:false,revoked:false,can_apply:true,can_remove:true,node_count:0,slurm_gpu_nodes:0,setup_command:'curl x | sh',source:{provider_id:3,provider_name:'ali-prod',provider_kind:'aliyun',provider_cluster_id:'ehpc-sz-1'}};
const group=(state)=>e(TableGroup,{name:'gpu-lab',colSpan:4,open:false,onToggle:()=>{},title:e('strong',null,'gpu-lab'),extra:e(React.Fragment,null,e(SlurmEntryStatus,{cluster:{...entry,state}}),e(SlurmEntryActions,{cluster:{...entry,state},onChanged:()=>{},labeled:true}))});
createRoot(document.getElementById('root')).render(e(I18nextProvider,{i18n},e(MemoryRouter,null,e(Table,null,group('pending'),group('active')))));
window.ready=true;
</script></body></html>`;
let server, browser;
try {
  await writeFile(fixture, html);
  server = await createServer({ root, server: { host: '127.0.0.1', port: 0 } }); await server.listen();
  browser = await chromium.launch(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH, headless: true } : { channel: 'chrome', headless: true });
  for (const theme of ['light', 'dark']) for (const locale of ['zh', 'en']) for (const width of [1440, 1024]) {
    const page = await browser.newPage({ viewport: { width, height: 500 } });
    await page.goto(server.resolvedUrls.local[0] + '.__labeled-row-actions.html?theme=' + theme + '&locale=' + locale);
    await page.waitForFunction(() => window.ready && document.querySelectorAll('[role=group] button').length >= 6);
    const buttons = await page.evaluate(() => [...document.querySelectorAll('[role=group] button')].map((button) => {
      const box = button.getBoundingClientRect(), icon = button.querySelector('svg')?.getBoundingClientRect(), label = button.querySelector('[data-slot=button-label]'), text = label?.getBoundingClientRect();
      return { name: button.textContent, height: box.height, iconLeftOfText: !!icon && !!text && icon.right <= text.left + 1 && Math.abs((icon.top + icon.bottom) / 2 - (text.top + text.bottom) / 2) < 6,
        textFits: !!label && label.scrollWidth <= label.clientWidth + 1 && text.left >= box.left && text.right <= box.right + 1 && text.top >= box.top - 1 && text.bottom <= box.bottom + 1, small: box.height <= 36 };
    }));
    const where = JSON.stringify({ theme, locale, width });
    assert.ok(buttons.length >= 6, 'both headers carry their buttons: ' + where);
    for (const button of buttons) {
      assert.ok(button.iconLeftOfText, 'icon left of the name, on one line: ' + JSON.stringify(button) + where);
      assert.ok(button.textFits, 'the name is not clipped: ' + JSON.stringify(button) + where);
      assert.ok(button.small, 'as tall as a small control: ' + JSON.stringify(button) + where);
    }
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    assert.ok(overflow <= 1, 'no horizontal page scroll: ' + where);
    await page.screenshot({ path: resolve(out, `labeled-${theme}-${locale}-${width}.png`) });
    await page.close();
  }
  console.log('labelled row actions: ok');
} finally { await browser?.close(); await server?.close(); await unlink(fixture).catch(() => {}); }
