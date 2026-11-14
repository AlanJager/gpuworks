#!/usr/bin/env python3
"""Real component geometry with offline catalog fixtures; no management server.

Build UI first, then run make test-ui-template-catalog-layout. Fixture counts
and hardware names are synthetic stress data, not product support claims.
"""
import os
from pathlib import Path
import subprocess
import unittest
from urllib.parse import urlparse

from playwright.sync_api import expect, sync_playwright
from reduced_motion import BUILD, UI

FIXTURE = r"""
const React = require('react');
const {createRoot} = require('react-dom/client');
const {MemoryRouter} = require('react-router-dom');
const {I18nextProvider} = require('react-i18next');
const {api} = require('./src/api');
const Page = require('./src/pages/InferenceTemplatesPage').default;
const {SideDrawer} = require('./src/ui-kit/SideDrawer');
const i18n = require('i18next').createInstance();
const h = React.createElement;
const vendors = ['nvidia', 'ascend', 'amd', 'hygon', 'ppu', 'kunlunxin'];
const chips = vendors.flatMap(vendor => Array.from({length:8}, (_,i) => ({vendor, name:`MOCK-${vendor}-${i}`})));
const models = ['black-forest-labs', 'thinkingmachines', 'deepseek-ai'].flatMap(org => Array.from({length:12}, (_,i) => ({
 id:`${org}-${i}`, model_id:`${org}/MOCK-long-model-name-${i}`, source:'upstream',
 specs:vendors.map((vendor,j)=>({id:j,name:`MOCK-spec-${j}`,engine:'vllm',quantization:'fp8',
 gpus:{vendor,name:`MOCK-${vendor}-0`},weight_sources:[{provider:'huggingface',repo_id:`${org}/MOCK-weights-${i}`}],
 api:{operations:[{type:'openai.chat'}]}}))
})));
api.inferenceTemplates = async () => ({items:models,total:models.length,page:1,page_size:200});
api.detectedInferenceTemplateChips = async () => ({items:chips});
api.inferenceTemplateChips = async () => ({items:chips});
const drawer = new URLSearchParams(location.search).has('drawer');
i18n.init({lng:'en',defaultNS:'inferenceTemplates',interpolation:{escapeValue:false},resources:{en:{
 inferenceTemplates:require('./src/i18n/locales/en/inferenceTemplates.json'),common:require('./src/i18n/locales/en/common.json')
}}}).then(()=>createRoot(document.getElementById('root')).render(h(I18nextProvider,{i18n},
 h(MemoryRouter,{initialEntries:['/inference-templates']},h('div',{style:{marginLeft:208,padding:24,minWidth:0}},
 drawer ? h(SideDrawer,{open:true,onClose(){},title:'Geometry drawer'},'Drawer body') : h(Page))))));
"""


class TemplateCatalogLayout(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bundle = subprocess.run(
            ["node", "-e", BUILD, str(UI / "node_modules/esbuild"), FIXTURE, str(UI)],
            capture_output=True, text=True, check=True,
        ).stdout
        css_files = list((UI.parent / "internal/server/webui/dist/assets").glob("*.css"))
        if not css_files:
            raise RuntimeError("Run npm run build in ui first")
        cls.css = "\n".join(path.read_text() for path in css_files)
        cls.playwright = sync_playwright().start()
        cls.browser = cls.playwright.chromium.launch(headless=True)

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()

    def render_page(self, width, dark=False, drawer=False):
        page = self.browser.new_page(viewport={"width": width, "height": 1000})
        self.addCleanup(page.close)

        def route(request):
            path = urlparse(request.request.url).path
            asset = UI / "public" / path.lstrip("/")
            if path.startswith("/assets/") and asset.is_file():
                request.fulfill(path=str(asset))
            elif path == "/":
                request.fulfill(content_type="text/html", body='<!doctype html><div id="root"></div>')
            else:
                request.abort()

        page.route("**/*", route)
        page.goto("http://template-layout.test/" + ("?drawer" if drawer else ""))
        if dark:
            page.evaluate("document.documentElement.classList.add('dark')")
        page.add_style_tag(content=self.css)
        page.add_script_tag(content=self.bundle)
        return page

    def capture(self, page, name):
        output = os.environ.get("GPUWORKS_LAYOUT_SCREENSHOT_DIR")
        if output:
            target = Path(output)
            target.mkdir(parents=True, exist_ok=True)
            page.screenshot(path=str(target / name), full_page=True)

    def test_dense_catalog_owns_scroll_and_keeps_names_and_logos(self):
        for width in (1280, 1920):
            for dark in (False, True):
                with self.subTest(width=width, dark=dark):
                    page = self.render_page(width, dark)
                    expect(page.locator('[data-slot="table"]')).to_be_visible()
                    expect(page.locator('.template-hardware-group > .vendor-logo')).to_have_count(6)
                    for name in ('Black Forest Labs', 'Thinking Machines'):
                        label = page.locator('.template-organization-sidebar nav strong').filter(has_text=name)
                        expect(label).to_be_visible()
                        self.assertTrue(label.evaluate('el => el.scrollWidth <= el.clientWidth + 1'))
                    chip_label = page.locator('.template-hardware-filters > .template-filter-label').bounding_box()
                    first_brand = page.locator('.template-hardware-group').first.bounding_box()
                    self.assertLess(abs(chip_label['y'] - first_brand['y']), 8)
                    self.assertGreater(first_brand['x'], chip_label['x'] + chip_label['width'])
                    expect(page.get_by_text('Model type', exact=True)).to_be_visible()
                    heights = page.locator('[data-slot="table-head"]').evaluate_all('els=>els.map(el=>el.getBoundingClientRect().height)')
                    self.assertTrue(all(abs(height-44) <= 1 for height in heights), heights)
                    self.assertTrue(page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'))
                    container = page.locator('[data-slot="table-container"]')
                    if width == 1280:
                        self.assertTrue(container.evaluate('el=>el.scrollWidth > el.clientWidth'))
                        container.evaluate('el=>el.scrollLeft=200')
                        self.assertGreater(container.evaluate('el=>el.scrollLeft'), 0)
                    name_cell = page.locator('.resource-table-name').first
                    self.assertGreaterEqual(name_cell.bounding_box()['x'], container.bounding_box()['x'] - 1)
                    self.capture(page, f'templates-{width}-{"dark" if dark else "light"}.png')

    def test_collapsing_filters_returns_space_to_the_catalog(self):
        page = self.render_page(1280)
        table = page.locator('[data-slot="table"]')
        expect(table).to_be_visible()
        before = table.bounding_box()['y']
        toggle = page.get_by_role('button', name='Filters', exact=True)
        expect(toggle).to_have_attribute('aria-expanded', 'true')
        toggle.click()
        expect(toggle).to_have_attribute('aria-expanded', 'false')
        expect(page.locator('.template-hardware-filters')).not_to_be_visible()
        self.assertLess(table.bounding_box()['y'], before - 80)
        toggle.click()
        expect(page.locator('.template-hardware-filters')).to_be_visible()

    def test_drawer_has_square_corners(self):
        page = self.render_page(1280, drawer=True)
        dialog = page.get_by_role('dialog', name='Geometry drawer')
        expect(dialog).to_be_visible()
        corners = dialog.evaluate('el=>{const s=getComputedStyle(el);return [s.borderTopLeftRadius,s.borderTopRightRadius,s.borderBottomLeftRadius,s.borderBottomRightRadius]}')
        self.assertEqual(corners, ['0px'] * 4)


if __name__ == '__main__':
    unittest.main()
