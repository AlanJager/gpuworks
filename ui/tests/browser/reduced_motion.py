#!/usr/bin/env python3
"""Browser component regression for popup geometry under reduced motion.

Run `make test-ui-motion` with UI dependencies, Python Playwright and its
Chromium installed. Uses the real Ant Design components and application CSS;
no server, network fixtures or GPU are involved. This checks shared layout,
not the management API or a complete product workflow.
"""

import re
import subprocess
import unittest
from pathlib import Path

from playwright.sync_api import expect, sync_playwright


UI = Path(__file__).resolve().parents[2]
FIXTURE = """
const React = require('react');
const {createRoot} = require('react-dom/client');
const {Button, ConfigProvider, DatePicker, Dropdown, Modal, Select} = require('antd');
const {themeConfig} = require('./src/theme/tokens');
const h = React.createElement;
function App() {
  const [value, setValue] = React.useState('none');
  const [open, setOpen] = React.useState(false);
  return h(ConfigProvider, {theme: themeConfig}, h('main', {style: {margin: 100}},
    h(Select, {id: 'model', 'aria-label': 'Model', style: {width: 320},
      options: [{value: 'flash', label: 'Flash'}], onChange: setValue}),
    h(Dropdown, {trigger: ['click'], menu: {
      items: [{key: 'export', label: 'Export results'}],
      onClick: ({key}) => setValue(key),
    }}, h(Button, null, 'Actions')),
    h(DatePicker, {'aria-label': 'Date', onChange: (_, date) => setValue(date)}),
    h(Button, {onClick: () => setOpen(true)}, 'Open dialog'),
    h(Modal, {title: 'Test dialog', open, onCancel: () => setOpen(false),
      onOk: () => {setValue('confirmed'); setOpen(false);}}, 'Dialog content'),
    h('output', {'data-testid': 'selection'}, value)));
}
createRoot(document.getElementById('root')).render(h(App));
"""
BUILD = """
require(process.argv[1]).build({
  stdin: {contents: process.argv[2], resolveDir: process.argv[3]},
  bundle: true, write: false, platform: 'browser',
  define: {'process.env.NODE_ENV': '"production"'},
}).then(result => process.stdout.write(result.outputFiles[0].text))
  .catch(error => {console.error(error); process.exit(1);});
"""


class ReducedMotionPopups(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bundle = subprocess.run(
            ["node", "-e", BUILD, str(UI / "node_modules/esbuild"), FIXTURE, str(UI)],
            check=True, capture_output=True, text=True,
        ).stdout
        cls.css = (UI / "src/styles.css").read_text()
        cls.playwright = sync_playwright().start()
        cls.browser = cls.playwright.chromium.launch()

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()

    def assert_popup(self, page, selector, anchor=None):
        popup = page.locator(selector)
        expect(popup).to_be_visible()
        # Visibility alone accepts an offscreen popup. Require real geometry
        # near the trigger, without a resize event that could mask first-open bugs.
        page.wait_for_function(
            """({selector, anchor}) => {
              const el = document.querySelector(selector);
              if (!el) return false;
              const r = el.getBoundingClientRect();
              if (!(r.width > 0 && r.height > 0 && r.left >= 0 && r.top >= 0 &&
                    r.right <= innerWidth && r.bottom <= innerHeight)) return false;
              if (!anchor) return true;
              const a = document.querySelector(anchor).getBoundingClientRect();
              return Math.abs(r.left - a.left) < 20 && Math.abs(r.top - a.bottom) < 20;
            }""",
            arg={"selector": selector, "anchor": anchor}, timeout=3000,
        )

    def exercise(self, motion):
        page = self.browser.new_page(
            reduced_motion=motion, viewport={"width": 1317, "height": 799},
        )
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        try:
            page.set_content('<!doctype html><html><head></head><body><div id="root"></div></body></html>')
            page.add_style_tag(content=self.css)
            page.add_script_tag(content=self.bundle)
            for _ in range(2):
                page.locator(".ant-select-selector").click()
                self.assert_popup(page, ".ant-select-dropdown:not(.ant-select-dropdown-hidden)", ".ant-select")
                page.locator(".ant-select-item-option-content").get_by_text("Flash", exact=True).click()
                expect(page.get_by_test_id("selection")).to_have_text("flash")

            page.get_by_role("button", name="Actions", exact=True).click()
            self.assert_popup(page, ".ant-dropdown:not(.ant-dropdown-hidden)")
            page.get_by_role("menuitem", name="Export results").click()
            expect(page.get_by_test_id("selection")).to_have_text("export")

            page.get_by_role("textbox", name="Date", exact=True).click()
            self.assert_popup(page, ".ant-picker-dropdown:not(.ant-picker-dropdown-hidden)", ".ant-picker")
            page.locator(".ant-picker-cell-in-view").filter(has_text="15").first.click()
            expect(page.get_by_test_id("selection")).to_have_text(re.compile(r"\d{4}-\d{2}-15"))

            page.get_by_role("button", name="Open dialog").click()
            self.assert_popup(page, ".ant-modal")
            page.get_by_role("button", name="OK", exact=True).click()
            expect(page.get_by_role("dialog")).not_to_be_visible()
            expect(page.get_by_test_id("selection")).to_have_text("confirmed")
            self.assertEqual(errors, [])
        finally:
            page.close()

    def test_normal_motion(self):
        self.exercise("no-preference")

    def test_reduced_motion(self):
        self.exercise("reduce")


if __name__ == "__main__":
    unittest.main()
