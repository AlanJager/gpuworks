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
import { themeConfig } from '/src/theme/tokens';
import '/src/app.css';
import '/src/theme/layers.css';
import '/src/components/detail/detailDrawer.css';
import zhStorage from '/src/i18n/locales/zh/storage.json';
import zhCommon from '/src/i18n/locales/zh/common.json';
const query = new URLSearchParams(location.search);
const i18n = createInstance();
const uuid = 'mock-long-node-1234567890abcdef1234567890abcdef';
const message = 'Download failed while contacting registry; retry after checking the network. https://registry.example.invalid/models/' + 'long-path-segment/'.repeat(12);
const storage = {id:1, name:'mock-nfs', type:'nfs', enabled:true, is_default:true, all_nodes:true, path:'/models', targets:[], model_count:0, deployment_ids:[], writable:true, nodes:{total:2,ready:1,problems:[],all:[{node_uuid:uuid,node_name:'mock-host-with-a-long-name',state:'ready',writable:true,total_bytes:117*1024**4,available_bytes:23*1024**4,path:'/models',message},{node_uuid:'mock-other',node_name:'mock-other',state:'failed',writable:false,message}]}};
const noop = ()=>{};
i18n.init({lng:'zh',fallbackLng:'zh',interpolation:{escapeValue:false},resources:{zh:{storage:zhStorage,common:zhCommon}}}).then(()=>createRoot(document.getElementById('root')).render(
 <StyleProvider layer><ConfigProvider theme={{algorithm:query.has('dark')?theme.darkAlgorithm:theme.defaultAlgorithm,...themeConfig}}><I18nextProvider i18n={i18n}><MemoryRouter>
 <DetailDrawer open title='Mock storage' onClose={noop}>
 <StorageNodesTab storage={storage} files={[]}/>
 </DetailDrawer></MemoryRouter></I18nextProvider></ConfigProvider></StyleProvider>));
'''

class StorageImageLayout(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix='.detail-layout-', dir=UI)
        root = Path(cls.temp.name)
        (root/'fixture.tsx').write_text(FIXTURE)
        (root/'index.html').write_text('''<!doctype html><html><head><script>if(new URLSearchParams(location.search).has('dark')){document.documentElement.classList.add('dark');document.documentElement.dataset.theme='dark'}</script></head><body><div id="root"></div><script type="module" src="./fixture.tsx"></script></body></html>''')
        for cls.port in [8800, 8802, 8804, 8806]:
            with socket.socket() as probe:
                try: probe.bind(('127.0.0.1',cls.port))
                except OSError: continue
                break
        cls.server = subprocess.Popen(['node',str(UI/'node_modules/vite/bin/vite.js'),'--host','127.0.0.1','--port',str(cls.port),'--strictPort'],cwd=UI,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        cls.url=f'http://127.0.0.1:{cls.port}/{root.name}/index.html'
        for _ in range(100):
            try:
                with urlopen(cls.url,timeout=1): break
            except OSError: time.sleep(.1)
        else: raise RuntimeError('owned Vite did not start')
        cls.pw=sync_playwright().start()
        cls.browser=cls.pw.chromium.launch(headless=True)

    @classmethod
    def tearDownClass(cls):
        cls.browser.close(); cls.pw.stop()
        cls.server.terminate(); cls.server.wait(timeout=10); cls.temp.cleanup()

    def render(self,width,dark):
        page=self.browser.new_page(viewport={'width':width,'height':1000})
        self.addCleanup(page.close)
        errors=[]
        page.on('pageerror',lambda e:errors.append(str(e)))
        page.goto(self.url+ '?storage'+ ('&dark' if dark else ''),wait_until='domcontentloaded')
        expect(page.get_by_role('dialog')).to_be_visible()
        expect(page.get_by_role('button',name='收起')).to_have_count(0)
        expect(page.get_by_role('link',name='mock-host-with-a-long-name')).to_be_visible()
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

if __name__=='__main__': unittest.main()
