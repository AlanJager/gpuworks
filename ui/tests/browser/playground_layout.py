#!/usr/bin/env python3
"""Chromium component layout regression; no GPU or live management API.

Run make test-ui-playground-layout. Renders the real Playground and CSS with
an offline catalog/SSE fixture, checking geometry rather than jsdom layout.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest

from playwright.sync_api import expect, sync_playwright

from reduced_motion import BUILD, UI


FIXTURE = """
const React = require('react');
const {createRoot} = require('react-dom/client');
const {ConfigProvider, Layout} = require('antd');
const {MemoryRouter} = require('react-router-dom');
const {I18nextProvider} = require('react-i18next');
const i18n = require('i18next').createInstance();
const Playground = require('./src/pages/playground/PlaygroundPage').default;
const {themeConfig} = require('./src/theme/tokens');
const h = React.createElement;
const locale = new URL(location.href).searchParams.get('locale') || 'en';
i18n.init({lng: locale, defaultNS: 'common', resources: {
  en: {common: require('./src/i18n/locales/en/common.json'), playground: require('./src/i18n/locales/en/playground.json')},
  zh: {common: require('./src/i18n/locales/zh/common.json'), playground: require('./src/i18n/locales/zh/playground.json')},
}, interpolation: {escapeValue: false}}).then(() => {
  createRoot(document.getElementById('root')).render(
    h(I18nextProvider, {i18n}, h(ConfigProvider, {theme: themeConfig},
      h(MemoryRouter, {initialEntries: ['/playground/chat?model=layout-model']},
        h(Layout, {className: 'app-shell'},
          h(Layout.Header, {className: 'app-header'}, 'GPUWorks'),
          h(Layout, null,
            h(Layout.Sider, {width: 208, breakpoint: 'lg', collapsedWidth: 0}),
            h(Layout.Content, {className: 'app-content'}, h(Playground))))))));
});
"""

CATALOG = {
    "models": [{"id": "layout-model", "label": "layout-model", "capability": "chat",
                "sleeping": False, "public_data_plane": True,
                "operations": [{"type": "openai.chat", "endpoint": "/v1/chat/completions",
                                "renderer": "chat", "response": "sse", "mode": "stream",
                                "request_encodings": ["json"]}]}],
    "parameters": [], "extra_parameter_presets": [], "stream_options": [],
    "html_preview": {"allow_network": False},
}


class PlaygroundLayout(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bundle = subprocess.run(
            ["node", "-e", BUILD, str(UI / "node_modules/esbuild"), FIXTURE, str(UI)],
            check=True, capture_output=True, text=True,
        ).stdout
        cls.css = (UI / "src/styles.css").read_text() + (UI / "src/pages/playground/playground.css").read_text()
        cls.playwright = sync_playwright().start()
        cls.browser = cls.playwright.chromium.launch(executable_path=shutil.which("google-chrome"))

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()

    def exercise(self, width, height, locale="en", compare=False, sleeping=False, expanded=False):
        page = self.browser.new_page(viewport={"width": width, "height": height})
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        labels = json.loads((UI / f"src/i18n/locales/{locale}/playground.json").read_text())
        answer = "Paragraph of a long reply.\n\n" * 60 + "LAST ANSWER LINE"
        frames = [{"choices": [{"delta": {"content": answer}}]},
                  {"choices": [{"delta": {}, "finish_reason": "stop"}],
                   "usage": {"prompt_tokens": 10, "completion_tokens": 100, "total_tokens": 110}}]
        sse = "".join("data: " + json.dumps(frame) + "\n\n" for frame in frames) + "data: [DONE]\n\n"

        def route(request):
            path = request.request.url.split("layout.test", 1)[-1].split("?", 1)[0]
            if path == "/api/v1/playground/models":
                catalog = json.loads(json.dumps(CATALOG))
                catalog["models"][0]["sleeping"] = sleeping
                request.fulfill(json=catalog)
            elif path == "/api/v1/auth/me":
                request.fulfill(json={"id": 1, "username": "admin", "role": "admin"})
            elif path == "/api/v1/playground/v1/chat/completions":
                request.fulfill(content_type="text/event-stream", body=sse)
            elif path == "/":
                request.fulfill(content_type="text/html", body='<!doctype html><div id="root"></div>')
            elif path == "/react-image-crop/dist/ReactCrop.css":
                request.fulfill(content_type="text/css", body=(UI / "node_modules/react-image-crop/dist/ReactCrop.css").read_text())
            else:
                request.abort()
                raise AssertionError(f"Unexpected fixture request: {path}")

        try:
            page.route("**/*", route)
            page.goto(f"http://layout.test/?locale={locale}")
            page.add_style_tag(content=self.css)
            page.add_script_tag(content=self.bundle)
            composer = page.get_by_role("textbox", name=labels["chat"]["inputAria"])
            expect(composer).to_be_visible()
            if compare:
                page.get_by_text(labels["chat"]["compare"], exact=True).click()
            composer.fill("A long answer please")
            composer.press("Enter")
            expect(page.locator(".playground-metrics").first).to_be_visible()
            for item in page.locator(".playground-message-list").all():
                item.evaluate("el => el.scrollTop = el.scrollHeight")
            if expanded:
                composer.fill("Draft line\n" * 6)
                # Ant Design autosizes on its next render; check geometry only
                # after the textarea has grown, then reveal the reply's tail.
                page.wait_for_function("document.querySelector('textarea').clientHeight > 100")
                page.locator(".playground-message-list").evaluate("el => el.scrollTop = el.scrollHeight")
            # Capture even the failing baseline for a visual diagnosis.
            if directory := os.environ.get("PLAYGROUND_SCREENSHOTS"):
                Path(directory).mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(Path(directory) / f"{width}-{height}-{locale}-{compare}.png"))
            geometry = page.evaluate("""() => {
              const rect = el => { const r = el.getBoundingClientRect(); return {top:r.top,bottom:r.bottom,height:r.height}; };
              return {composer: rect(document.querySelector('.playground-composer')),
                panes: [...document.querySelectorAll('.playground-chat-pane')].map(rect),
                lists: [...document.querySelectorAll('.playground-message-list')].map(rect),
                metrics: [...document.querySelectorAll('.playground-metrics')].map(rect)};
            }""")
            self.assertLessEqual(geometry["composer"]["bottom"], height + 1, geometry)
            for pane, messages, metrics in zip(geometry["panes"], geometry["lists"], geometry["metrics"]):
                self.assertGreater(messages["height"], 60, geometry)
                self.assertLessEqual(pane["bottom"], geometry["composer"]["top"], geometry)
                self.assertGreaterEqual(metrics["top"], messages["top"], geometry)
                self.assertLessEqual(metrics["bottom"], messages["bottom"] + 1, geometry)
            self.assertEqual(errors, [])
        finally:
            page.close()

    def test_short_single(self):
        self.exercise(1280, 720, "zh")

    def test_short_compare(self):
        self.exercise(1280, 720, compare=True)

    def test_desktop(self):
        self.exercise(1920, 1080)

    def test_narrow(self):
        self.exercise(768, 900)

    def test_growing_composer(self):
        self.exercise(1280, 720, expanded=True)

    def test_sleep_alert(self):
        self.exercise(1280, 720, sleeping=True)


if __name__ == "__main__":
    unittest.main()
