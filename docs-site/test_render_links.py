import os
import shutil
import subprocess
import tempfile
import unittest
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urljoin


class Links(HTMLParser):
    def __init__(self, html):
        super().__init__(convert_charrefs=True)
        self.links, self.current = {}, None
        self.feed(html)

    def handle_starttag(self, tag, attrs):
        if tag == 'a':
            self.current = dict(attrs, text='', tags=[])
        elif self.current is not None:
            self.current['tags'].append(tag)

    def handle_data(self, text):
        if self.current is not None:
            self.current['text'] += text

    def handle_endtag(self, tag):
        if tag == 'a' and self.current is not None:
            self.links[self.current['text']] = self.current
            self.current = None


class RenderLinksTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        hugo = os.environ.get('HUGO_BIN') or shutil.which('hugo')
        if not hugo:
            raise RuntimeError('Hugo is required: set HUGO_BIN or install hugo on PATH')
        cls.temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temp.cleanup)
        root = Path(cls.temp.name)

        def write(name, text):
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text, encoding='utf-8')

        write('hugo.toml', '''baseURL = "https://example.invalid/docs/"
disableKinds = ["taxonomy", "term", "rss", "sitemap"]
defaultContentLanguage = "zh"
[languages.zh]
weight = 1
[languages.en]
weight = 2
[params]
github_repo = "https://example.invalid/team/gpuworks"
github_branch = "review-branch"
artifacts_repo = "https://example.invalid/team/gpuworks-artifacts"
''')
        for kind in ['single', 'list']:
            write(f'layouts/_default/{kind}.html', '<main>{{ .Content }}</main>')
        hook = Path(__file__).parent / 'layouts/_markup/render-link.html'
        write('layouts/_markup/render-link.html', hook.read_text(encoding='utf-8'))
        write('content/_index.md', '---\ntitle: Home\n---\n')
        write('content/reference/_index.md', '---\ntitle: Reference\n---\n## 索引\n')
        write('content/reference/target.md', '---\ntitle: Target\n---\n## 参数说明\n')
        write('static/assets/examples/prompts.jsonl', '{"prompt":"hello"}\n')
        write('content/how-to/source.md', '''---
title: Source
---
## 本页标题

[same](#本页标题)
[ordinary](../reference/target.md#参数说明)
[home](../README.md)
[directory-home](../)
[section](../reference/README.md#索引)
[repo](../../CONTEXT.md)
[artifact](../../artifacts/work/demo/plan.md#详细说明)
[encoded](../../artifacts/work/demo/plan.md?view=full#%E8%AF%A6%E7%BB%86%E8%AF%B4%E6%98%8E)
[query](../reference/target.md?view=full#%E5%8F%82%E6%95%B0%E8%AF%B4%E6%98%8E)
[external](https://other.invalid/a?x=1#part)
[mail](mailto:docs@example.invalid)
[missing](./missing.md)
[download](../../assets/examples/prompts.jsonl?download=1#sample)
[**说明** `flag`](../reference/target.md "标题说明")
''')
        write('content/dev/ledgers/deep.md', '''---
title: Deep
---
[deep-home](../../README.md)
[deep-directory-home](../../)
[deep-doc](../../reference/target.md)
[deep-repo](../../../CONTEXT.md)
[deep-download](../../../assets/examples/prompts.jsonl)
''')
        write('content/_index.en.md', '---\ntitle: Home\n---\n')
        write('content/reference/_index.en.md', '---\ntitle: Reference\n---\n## Index {#索引}\n')
        write('content/reference/target.en.md', '---\ntitle: Target\n---\n## Parameters {#参数说明}\n')
        write('content/reference/chinese-only.md', '---\ntitle: Chinese only\n---\n## 条件\n')
        write('content/how-to/source.en.md', '''---
title: Source
source_path: how-to/source.md
---
## Current heading {#本页标题}

[same](#本页标题)
[ordinary](../reference/target.md#参数说明)
[section](../reference/README.md#索引)
[home](../README.md)
[fallback](../reference/chinese-only.md#条件)
[download](../../../assets/examples/prompts.jsonl?download=1#sample)
[repo](../../CONTEXT.md)
[artifact](../../artifacts/work/demo/plan.md#详细说明)
''')
        result = subprocess.run([hugo, '--source', str(root), '--panicOnWarning'],
                                capture_output=True, text=True, timeout=30)
        if result.returncode:
            raise AssertionError(f'Hugo exited {result.returncode}:\n{result.stdout}{result.stderr}')
        cls.source = Links((root / 'public/how-to/source/index.html').read_text()).links
        cls.english = Links((root / 'public/en/how-to/source/index.html').read_text()).links
        cls.deep = Links((root / 'public/dev/ledgers/deep/index.html').read_text()).links
        cls.repo = 'https://example.invalid/team/gpuworks/blob/review-branch/'
        cls.artifacts = 'https://example.invalid/team/gpuworks-artifacts/blob/review-branch/'

    def test_local_anchor_and_normal_markdown_page(self):
        actual = urljoin('https://example.invalid/docs/how-to/source/', self.source['same']['href'])
        self.assertEqual(unquote(actual), 'https://example.invalid/docs/how-to/source/#本页标题')
        self.assertEqual(unquote(self.source['ordinary']['href']), '/docs/reference/target/#参数说明')

    def test_readme_links_resolve_to_home_and_section(self):
        self.assertEqual(self.source['home']['href'], '/docs/')
        self.assertEqual(unquote(self.source['section']['href']), '/docs/reference/#索引')

    def test_deep_source_resolves_relative_to_its_own_directory(self):
        for name, expected in [('deep-home', '/docs/'), ('deep-doc', '/docs/reference/target/'),
                               ('deep-repo', self.repo + 'CONTEXT.md')]:
            with self.subTest(name=name):
                self.assertEqual(self.deep[name]['href'], expected)

    def test_document_root_directory_is_not_a_repository_file(self):
        self.assertEqual(self.source['directory-home']['href'], '/docs/')
        self.assertEqual(self.deep['deep-directory-home']['href'], '/docs/')

    def test_repository_files_and_artifact_fragments(self):
        self.assertEqual(self.source['repo']['href'], self.repo + 'CONTEXT.md')
        self.assertEqual(unquote(self.source['artifact']['href']), self.artifacts + 'work/demo/plan.md#详细说明')

    def test_staged_downloads_use_published_assets_not_repository_urls(self):
        self.assertEqual(self.source['download']['href'], '/docs/assets/examples/prompts.jsonl?download=1#sample')
        self.assertEqual(self.deep['deep-download']['href'], '/docs/assets/examples/prompts.jsonl')

    def test_query_and_encoded_fragments_are_not_lost_or_double_encoded(self):
        for name, expected in [('encoded', self.artifacts + 'work/demo/plan.md?view=full#详细说明'),
                               ('query', '/docs/reference/target/?view=full#参数说明')]:
            with self.subTest(name=name):
                self.assertEqual(unquote(self.source[name]['href']), expected)

    def test_external_mail_and_missing_document_destinations_are_preserved(self):
        for name, expected in [('external', 'https://other.invalid/a?x=1#part'),
                               ('mail', 'mailto:docs@example.invalid'), ('missing', './missing.md')]:
            with self.subTest(name=name):
                self.assertEqual(self.source[name]['href'], expected)

    def test_link_title_and_inline_markup_survive(self):
        link = self.source['说明 flag']
        self.assertEqual(link['href'], '/docs/reference/target/')
        self.assertEqual(link['title'], '标题说明')
        self.assertEqual(link['tags'], ['strong', 'code'])

    def test_english_links_stay_in_language_and_use_shared_assets(self):
        for name, expected in [
            ('same', '/docs/en/how-to/source/#本页标题'),
            ('ordinary', '/docs/en/reference/target/#参数说明'),
            ('section', '/docs/en/reference/#索引'),
            ('home', '/docs/en/'),
            ('download', '/docs/assets/examples/prompts.jsonl?download=1#sample'),
            ('repo', self.repo + 'CONTEXT.md'),
            ('artifact', self.artifacts + 'work/demo/plan.md#详细说明'),
        ]:
            with self.subTest(name=name):
                self.assertEqual(unquote(self.english[name]['href']), expected)

    def test_untranslated_page_fallback_is_explicit(self):
        link = self.english['fallback (Chinese)']
        self.assertEqual(unquote(link['href']), '/docs/reference/chinese-only/#条件')
        self.assertEqual(link['hreflang'], 'zh-CN')
