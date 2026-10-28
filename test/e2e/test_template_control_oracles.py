"""Offline candidate verification; no browser/server/product pass claimed."""
import unittest
from template_control_oracles import geometry, import_wire, changed_title, cleanup_owned, finish_stage, archive_members


def box(x, y, width, height):
    return dict(x=x, y=y, width=width, height=height)


class Oracles(unittest.TestCase):
    def regions(self):
        return dict(drawer=box(580, 0, 860, 900), header=box(580, 0, 860, 60),
                    title=box(640, 15, 200, 24), actions=box(1150, 12, 240, 36),
                    body=box(580, 60, 860, 840), editor=box(604, 84, 812, 700),
                    save=box(1290, 12, 100, 36), reset=box(1150, 12, 100, 36))

    def test_geometry_uses_actual_header_actions_not_invented_footer(self):
        geometry(self.regions(), dict(width=1440, height=900))
        for change in ('overlap', 'outside', 'empty', 'missing'):
            with self.subTest(change=change), self.assertRaises(AssertionError):
                data = self.regions()
                if change == 'overlap':
                    data['title'] = box(1200, 15, 160, 24)
                elif change == 'outside':
                    data['editor']['height'] = 1000
                elif change == 'empty':
                    data['save']['width'] = 0
                else:
                    data.pop('reset')
                geometry(data, dict(width=1440, height=900))

    def test_exact_import_contract_rejects_protocol_drift(self):
        good = ['POST', 'http://127.0.0.1/api/v1/inference-templates/import?overwrite=true',
                'application/yaml', 'display_title: "new"\n', 'display_title: "new"\n']
        import_wire(*good)
        for index, bad in ((0, 'PUT'), (1, good[1] + '&extra=1'), (2, 'text/plain'), (3, 'old')):
            with self.subTest(index=index), self.assertRaises(AssertionError):
                args = good.copy()
                args[index] = bad
                import_wire(*args)

    def test_actions_cannot_pass_while_covering_editor_or_leaving_header_actions(self):
        for name in ('save', 'reset'):
            for region in (box(650, 200, 100, 36), box(850, 12, 100, 36)):
                with self.subTest(name=name, region=region), self.assertRaisesRegex(AssertionError, 'outside actions'):
                    data = self.regions()
                    data[name] = region
                    geometry(data, dict(width=1440, height=900))

    def test_title_rewrite_is_strict_and_preserves_other_bytes(self):
        for encoding in ('old', '"old"'):
            self.assertEqual(changed_title('  display_title: ' + encoding + '\n  args: []\n', 'old', 'e2e-new'),
                             '  display_title: e2e-new\n  args: []\n')
        for text in ('display_title: old\n', '  display_title: "other"\n',
                     '  display_title: "old"\n  display_title: "old"\n'):
            with self.subTest(text=text), self.assertRaises(AssertionError):
                changed_title(text, 'old', 'e2e-new')

    def test_cleanup_partial_failure_keeps_residual_and_attempts_other_owned_models(self):
        owned, records, gone, calls = ['broken', 'good'], [], set(), []
        def fetch(model_id):
            return (404, {}) if model_id in gone else (200, {'source': 'user', 'specs': [{'source': 'user', 'name': 's'}]})
        def remove(model_id, spec):
            calls.append((model_id, spec))
            if model_id == 'broken':
                raise TimeoutError('held deletion')
            gone.add(model_id)
            return 200
        self.assertFalse(cleanup_owned(owned, fetch, remove, records.append))
        self.assertEqual(owned, ['broken'])
        self.assertEqual(calls, [('broken', 's'), ('good', 's')])
        self.assertEqual([r['ok'] for r in records], [False, True])

    def test_cleanup_does_not_treat_error_status_as_absence_or_delete_upstream(self):
        for status, model in ((500, {}), (200, {'source': 'upstream'}),
                              (200, {'source': 'user', 'specs': []}),
                              (200, {'source': 'user', 'specs': [{'source': 'upstream'}]})):
            with self.subTest(status=status, model=model):
                owned, calls = ['owned'], []
                self.assertFalse(cleanup_owned(owned, lambda _: (status, model),
                    lambda *args: calls.append(args), lambda _: None))
                self.assertEqual(owned, ['owned'])
                self.assertEqual(calls, [])

    def test_cleanup_close_failure_preserves_business_exception_and_continues(self):
        report = {'failure': 'original business assertion'}
        def broken():
            raise RuntimeError('close failed')
        self.assertFalse(finish_stage(report, 'context', broken))
        self.assertTrue(finish_stage(report, 'browser', lambda: True))
        self.assertEqual(report['failure'], 'original business assertion')
        self.assertEqual([x['ok'] for x in report['cleanup_steps']], [False, True])
        self.assertEqual(report['cleanup_errors'][0]['stage'], 'context')

    def test_unconfirmed_cleanup_cannot_be_a_pass(self):
        for value in (False, None, 0, 'true'):
            with self.subTest(value=value):
                report = {}
                self.assertFalse(finish_stage(report, 'cleanup', lambda: value))
                self.assertIn('did not confirm success', report['failure'])

    def test_package_comparison_uses_bytes_and_rejects_duplicate_or_escaping_entries(self):
        import io
        import tarfile
        def archive(names):
            raw = io.BytesIO()
            with tarfile.open(fileobj=raw, mode='w:gz') as target:
                for name in names:
                    entry = tarfile.TarInfo(name)
                    entry.size = 3
                    target.addfile(entry, io.BytesIO(b'abc'))
            return raw.getvalue()
        self.assertEqual(archive_members(archive(['manifest.yaml', 'models/e2e/model.yaml'])),
                         {'manifest.yaml': b'abc', 'models/e2e/model.yaml': b'abc'})
        for names in ([], ['model.yaml'], ['manifest.yaml', '../escape'],
                      ['manifest.yaml', '/absolute'], ['manifest.yaml', 'manifest.yaml']):
            with self.subTest(names=names), self.assertRaises(AssertionError):
                archive_members(archive(names))
        with self.assertRaises(tarfile.ReadError):
            archive_members(b'corrupt')


if __name__ == '__main__':
    unittest.main()
