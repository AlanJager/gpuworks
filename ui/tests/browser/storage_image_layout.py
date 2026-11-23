#!/usr/bin/env python3
"""Real production CSS/Chrome regressions for horizontally scrolled detail tables."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
from urllib.request import urlopen
from playwright.sync_api import expect, sync_playwright

UI = Path(__file__).resolve().parents[2]
FIXTURE = r'''
import React from 'react';
import { createRoot } from 'react-dom/client';
import { ConfigProvider, theme } from 'antd';
import { StyleProvider } from '@ant-design/cssinjs';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import DetailDrawer from '/src/components/detail/DetailDrawer';
import StorageNodesTab from '/src/pages/storage/StorageNodesTab';
import ImageNodeTable from '/src/pages/images/ImageNodeTable';
import { themeConfig } from '/src/theme/tokens';
import '/src/app.css';
import '/src/theme/layers.css';
import '/src/components/detail/detailDrawer.css';
import zhStorage from '/src/i18n/locales/zh/storage.json';
import zhImages from '/src/i18n/locales/zh/images.json';
import zhCommon from '/src/i18n/locales/zh/common.json';
const query = new URLSearchParams(location.search);
const image = query.has('image');
const i18n = createInstance();
const uuid = 'mock-long-node-1234567890abcdef1234567890abcdef';
const message = 'Download failed while contacting registry; retry after checking the network. https://registry.example.invalid/models/' + 'long-path-segment/'.repeat(12);
const storage = {id:1, name:'mock-nfs', type:'nfs', enabled:true, is_default:true, all_nodes:true, path:'/models', targets:[], model_count:0, deployment_ids:[], writable:true, nodes:{total:3,ready:1,problems:[],all:[{node_uuid:uuid,node_name:'mock-host-with-a-long-name',state:'ready',writable:true,total_bytes:117*1024**4,available_bytes:23*1024**4,path:'/models',message},{node_uuid:'mock-other',node_name:'mock-other',state:'failed',writable:false,message},{node_uuid:'mock-offline',node_name:'mock-offline',state:'offline',writable:false,last_seen_at:'2026-11-18T06:00:00Z'}]}};
const version = {id:1,series_id:1,name:'mock-runner',image_ref:'registry.example.invalid/team/runner:latest',source:'custom',enabled:true,reference_count:0,registry_sources:[],nodes:[{node_uuid:uuid,node_name:'mock-host',node_state:'online',state:'failed',message,image_digest_id:'sha256:'+ '1234567890abcdef'.repeat(4),updated_at:'2026-11-18T08:00:00Z',transfer_id:'mock-transfer-1234567890abcdef'}]};
window.EventSource = class {addEventListener(){} removeEventListener(){} close(){}};
// Faithful response shape from the read-only transfer observation endpoint.
window.unexpectedRequests = [];
window.fetch = async (input, init = {}) => {
  const request = input instanceof Request ? input : null;
  const url = new URL(request ? request.url : String(input), location.href);
  const method = String(init.method ?? request?.method ?? 'GET').toUpperCase();
  if (method !== 'GET' || url.origin !== location.origin || url.pathname !== '/api/v1/peer-transfers' ||
      [...url.searchParams].length !== 1 || url.searchParams.get('transfer_id') !== 'mock-transfer-1234567890abcdef') {
    const unexpected = `${method} ${url.href}`;
    window.unexpectedRequests.push(unexpected);
    throw new Error(`Unexpected fixture request: ${unexpected}`);
  }
  return new Response(JSON.stringify({observed_at:'2026-11-18T08:01:00Z',scope:'server_process',policy:{rate_limit_bps:67108864,burst_bytes:1048576,max_active:2,max_per_node:1,no_progress_timeout_seconds:60,confirmation_timeout_seconds:30},active_relay_count:0,rate_waiting_count:0,totals:{started:1,peer_confirmed:0,failed:0,canceled:0,busy:0,probe_started:0,probe_ok:0,probe_failed:0,source_to_target_bytes:0,target_to_source_bytes:0,rate_wait_ms:0},active:[{transfer_id:'mock-transfer-1234567890abcdef',kind:'image',operation:'sync',source_node_uuid:'mock-source',target_node_uuid:uuid,transport:'direct',phase:'connected',updated_at:'2026-11-18T08:01:00Z',source_to_target_bytes:0,target_to_source_bytes:0,effective_rate_limit_bps:0,server_rate_bps:0,rate_window_seconds:0,rate_wait_ms:0}]}),{headers:{'Content-Type':'application/json'}});
};
const noop = ()=>{};
i18n.init({lng:'zh',fallbackLng:'zh',interpolation:{escapeValue:false},resources:{zh:{storage:zhStorage,images:zhImages,common:zhCommon}}}).then(()=>createRoot(document.getElementById('root')).render(
 <StyleProvider layer><ConfigProvider theme={{algorithm:query.has('dark')?theme.darkAlgorithm:theme.defaultAlgorithm,...themeConfig}}><I18nextProvider i18n={i18n}><MemoryRouter>
 <DetailDrawer open title={image?'Mock image':'Mock storage'} onClose={noop}>
 {image?<ImageNodeTable selectedVersion={version} inspecting={null} availablePeer={()=>false} modeFor={()=>'auto'} setMode={noop} pullVersion={noop} probeImage={async()=>{}} syncFromPeer={async()=>{}} pullFromURL={async()=>{}} inspectVersion={noop} cancelTransfer={noop} removeFromNode={noop} repullVersion={noop}/>:<StorageNodesTab storage={storage} files={[]}/>}
 </DetailDrawer></MemoryRouter></I18nextProvider></ConfigProvider></StyleProvider>));
'''

class StorageImageLayout(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix='.detail-layout-', dir=UI)
        cls.addClassCleanup(cls.temp.cleanup)
        root = Path(cls.temp.name)
        (root/'fixture.tsx').write_text(FIXTURE)
        (root/'index.html').write_text('''<!doctype html><html><head><script>if(new URLSearchParams(location.search).has('dark')){document.documentElement.classList.add('dark');document.documentElement.dataset.theme='dark'}</script></head><body><div id="root"></div><script type="module" src="./fixture.tsx"></script></body></html>''')
        for cls.port in [8800, 8802, 8804, 8806]:
            with socket.socket() as probe:
                try: probe.bind(('127.0.0.1',cls.port))
                except OSError: continue
                break
        else:
            raise RuntimeError('no free browser-fixture port in 8800/8802/8804/8806')
        cls.server = subprocess.Popen(['node',str(UI/'node_modules/vite/bin/vite.js'),'--host','127.0.0.1','--port',str(cls.port),'--strictPort'],cwd=UI,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        cls.addClassCleanup(cls.stop_server, cls.server)
        cls.url=f'http://127.0.0.1:{cls.port}/{root.name}/index.html'
        for _ in range(100):
            if cls.server.poll() is not None:
                raise RuntimeError(f'owned Vite exited before readiness: {cls.server.returncode}')
            try:
                with urlopen(cls.url,timeout=1):
                    if cls.server.poll() is not None:
                        raise RuntimeError(f'owned Vite exited during readiness: {cls.server.returncode}')
                    break
            except OSError: time.sleep(.1)
        else: raise RuntimeError('owned Vite did not start')
        cls.pw=sync_playwright().start()
        cls.addClassCleanup(cls.pw.stop)
        cls.browser=cls.pw.chromium.launch(headless=True)
        cls.addClassCleanup(cls.browser.close)

    @staticmethod
    def stop_server(server):
        if server.poll() is None:
            server.terminate()
        try:
            server.wait(timeout=10)
        except subprocess.TimeoutExpired:
            server.kill()
            server.wait(timeout=10)

    def render(self,width,dark,image=False):
        page=self.browser.new_page(viewport={'width':width,'height':1000})
        self.addCleanup(page.close)
        errors=[]
        page.on('pageerror',lambda e:errors.append(str(e)))
        # Run before page.close (cleanups are LIFO), including failures after
        # expansion; caught API errors must not hide an unexpected request.
        self.addCleanup(self.assertEqual, errors, [])
        self.addCleanup(lambda: self.assertEqual(page.evaluate('window.unexpectedRequests'), []))
        page.goto(self.url+ ('?image' if image else '?storage')+ ('&dark' if dark else ''),wait_until='domcontentloaded')
        expect(page.get_by_role('dialog')).to_be_visible()
        expect(page.get_by_role('button',name='收起')).to_have_count(0)
        expect(page.get_by_text('mock-host',exact=True) if image else page.get_by_role('link',name='mock-host-with-a-long-name')).to_be_visible()
        page.wait_for_function("() => document.querySelectorAll('tbody td').length > 2")
        self.assertEqual(errors,[])
        return page

    def capture(self,page,name,metrics):
        output=os.environ.get('GPUWORKS_LAYOUT_SCREENSHOT_DIR')
        if output:
            target=Path(output); target.mkdir(parents=True,exist_ok=True)
            page.screenshot(path=str(target/(name+'.png')),full_page=True)
            (target/(name+'.json')).write_text(json.dumps(metrics,indent=2))

    def test_storage_status_stays_under_opaque_pinned_name_when_scrolled(self):
        for width in [1440,1024,560]:
            for dark in [False,True]:
                with self.subTest(width=width,dark=dark):
                    page=self.render(width,dark)
                    metrics=page.locator('table').evaluate('''table => {
                      const scroll=table.parentElement; scroll.scrollLeft=90;
                      const row=table.querySelector('tbody tr');
                      const name=row.querySelector('.resource-table-name'); const r=name.getBoundingClientRect();
                      const state=row.cells[2]; const s=state.getBoundingClientRect();
                      const top=document.elementFromPoint(r.right-2,r.top+r.height/2);
                      return {scroll:scroll.scrollLeft,bg:getComputedStyle(name).backgroundColor,position:getComputedStyle(state).position,nameAbove:!!top?.closest('.resource-table-name'),overlap:s.left<r.right,tableWidth:table.getBoundingClientRect().width,viewportWidth:scroll.clientWidth};
                    }''')
                    self.capture(page,f'storage-{width}-{dark}',metrics)
                    self.assertNotEqual(metrics['bg'],'rgba(0, 0, 0, 0)')
                    self.assertEqual(metrics['position'],'static')
                    if metrics['scroll']:
                        self.assertTrue(metrics['overlap']); self.assertTrue(metrics['nameAbove'])

    def test_offline_node_status_does_not_show_through_the_pinned_name(self):
        # An opaque background is not enough: a cell that is itself
        # translucent lets the cells scrolling under it show through.
        for width in [1440,560]:
            for dark in [False,True]:
                with self.subTest(width=width,dark=dark):
                    page=self.render(width,dark)
                    metrics=page.locator('table').evaluate('''table => {
                      table.parentElement.scrollLeft=120;
                      const row=table.querySelector('tbody tr.storage-node-offline');
                      const through=el=>{ let o=1; for (let e=el; e && e!==table; e=e.parentElement) o*=Number(getComputedStyle(e).opacity); return o; };
                      const pinned=[...row.querySelectorAll('td.resource-table-pinned')];
                      const state=row.cells[2];
                      return {offlineRow:!!row,pinned:pinned.length,pinnedOpacity:pinned.map(through),pinnedBackground:pinned.map(c=>getComputedStyle(c).backgroundColor),
                        pinnedContent:pinned.map(c=>c.firstElementChild?Number(getComputedStyle(c.firstElementChild).opacity):null),stateOpacity:through(state)};
                    }''')
                    self.capture(page,f'storage-offline-{width}-{dark}',metrics)
                    self.assertTrue(metrics['offlineRow']); self.assertGreaterEqual(metrics['pinned'],1)
                    self.assertEqual(metrics['pinnedOpacity'],[1]*metrics['pinned'],'a pinned cell of a dimmed row must stay opaque')
                    for background in metrics['pinnedBackground']: self.assertNotEqual(background,'rgba(0, 0, 0, 0)')
                    self.assertEqual([o for o in metrics['pinnedContent'] if o is not None],[0.65]*len([o for o in metrics['pinnedContent'] if o is not None]),'the pinned cell still reads as dimmed')
                    self.assertEqual(metrics['stateOpacity'],0.65)

    def test_image_expanded_facts_fit_visible_width_even_after_horizontal_scroll(self):
        for width in [1440,1024,560]:
            for dark in [False,True]:
                with self.subTest(width=width,dark=dark):
                    page=self.render(width,dark,True)
                    page.locator('.detail-expand').first.click()
                    expect(page.locator('.detail-extra').first).to_be_visible()
                    expect(page.locator('.image-node-extra section .detail-extra')).to_be_visible()
                    expect(page.get_by_text('传输连接已建立',exact=True)).to_be_visible()
                    for scrolled in [False,True]:
                        metrics=page.locator('.ant-table-content').evaluate('''scroll => {
                          scroll.scrollLeft=SCROLL ? scroll.scrollWidth : 0;
                          const dl=scroll.querySelector('.detail-extra'), r=dl.getBoundingClientRect(),v=scroll.getBoundingClientRect();
                          return {left:r.left,right:r.right,viewportLeft:v.left,viewportRight:v.right,scroll:scroll.scrollLeft,width:r.width,overflow:dl.scrollWidth>dl.clientWidth+1,wordBreak:getComputedStyle(dl.querySelector('dd')).wordBreak,background:getComputedStyle(dl.closest('td')).backgroundColor,textColor:getComputedStyle(dl.children[1].querySelector('dd')).color,allInside:[...scroll.querySelectorAll('.detail-extra')].every(x=>x.getBoundingClientRect().right<=v.right+1&&x.scrollWidth<=x.clientWidth+1),fields:[...dl.children].map(x=>({width:x.getBoundingClientRect().width,left:x.getBoundingClientRect().left,right:x.getBoundingClientRect().right}))};
                        }'''.replace('SCROLL','true' if scrolled else 'false'))
                        self.capture(page,f'image-{width}-{dark}-{scrolled}',metrics)
                        self.assertLessEqual(metrics['right'],metrics['viewportRight']+1)
                        self.assertGreaterEqual(metrics['left'],metrics['viewportLeft']-1)
                        self.assertFalse(metrics['overflow'])
                        self.assertTrue(metrics['allInside'])
                        if dark: self.assertNotEqual(metrics['background'],'rgb(245, 247, 250)')
                        self.assertNotEqual(metrics['wordBreak'],'break-all')

if __name__=='__main__': unittest.main()
