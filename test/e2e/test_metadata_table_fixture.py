"""Fail-closed corpus/pagination/ownership tests; not browser evidence."""
import copy
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
from metadata_table_fixture import OwnedCorpus, corpus, page_names, assert_traversal


class MetadataCorpusContracts(unittest.TestCase):
    def test_declared_totals_names_refs_and_strict_sizes(self):
        for total in (10,30,100,21):
            rows = corpus('owned',total,'sglang','rocm')
            self.assertEqual(len(rows),total)
            self.assertEqual(rows[0]['name'],'owned-000')
            self.assertEqual(rows[-1]['versions'][0]['image_ref'],f'registry.example/owned/runner:{total-1:03}')
            self.assertEqual(len({r['name'] for r in rows}),total)
            self.assertEqual({r['source'] for r in rows},{'custom'})
            self.assertEqual([r['enabled'] for r in rows[:2]],[True,False])
        for total in (0,9,20,99,101):
            with self.assertRaises(AssertionError): corpus('owned',total,'sglang','rocm')

    def test_traversals_reject_missing_duplicate_order_and_sliced_wrong_page(self):
        names = [r['name'] for r in corpus('owned',100,'sglang','rocm')]
        pages=[page_names(names,n,20) for n in range(1,6)]
        assert_traversal(pages,names,20)
        assert_traversal(list(reversed(pages)),names,20,backwards=True)
        changed=copy.deepcopy(pages);changed[2][0]=changed[1][0]
        for bad in (pages[:-1],changed,list(reversed(pages)),[p[:-1] for p in pages]):
            with self.assertRaises(AssertionError):assert_traversal(bad,names,20)
        for page,size in ((0,20),(1,30),(True,20)):
            with self.assertRaises(AssertionError):page_names(names,page,size)

    def owner(self):
        state={'remaining_resources':[{'kind':'image-series','id':1,'name':'own1'},
                                      {'kind':'image-series','id':2,'name':'own2'}]}
        page=SimpleNamespace(request=Mock())
        return OwnedCorpus(page,'http://owned',[],{'metadata_table':state}),state,page

    def test_delete_call_does_not_relinquish_ownership_without_observed_absence(self):
        owner,state,page=self.owner()
        owner.listed=Mock(return_value=[{'id':1}])
        with self.assertRaisesRegex(AssertionError,'series remains'):owner.confirm_gone(1)
        self.assertEqual(len(state['remaining_resources']),2)
        owner.listed=Mock(return_value=[])
        page.request.get.return_value=SimpleNamespace(status=200,json=lambda:{'items':[{'series_id':1}],'total':1})
        with self.assertRaisesRegex(AssertionError,'version remains'):owner.confirm_gone(1)
        self.assertEqual(len(state['remaining_resources']),2)

    def test_cleanup_attempts_other_owned_ids_and_retains_failed_one(self):
        owner,state,page=self.owner()
        page.request.delete.side_effect=[SimpleNamespace(status=500),SimpleNamespace(status=204)]
        owner.listed=Mock(return_value=[])
        page.request.get.side_effect=lambda url: (SimpleNamespace(status=200,json=lambda:{'items':[],'total':0})
            if url.endswith('/versions') else SimpleNamespace(status=404))
        with self.assertRaisesRegex(AssertionError,'cleanup stages failed'):owner.cleanup()
        self.assertEqual([r['id'] for r in state['remaining_resources']],[2])
        self.assertEqual(page.request.delete.call_count,2)
        self.assertEqual([x.args[0] for x in page.request.delete.call_args_list],
                         ['http://owned/api/v1/image-series/2','http://owned/api/v1/image-series/1'])

    def test_hidden_lists_do_not_prove_detail_deleted(self):
        owner,state,page=self.owner()
        owner.listed=Mock(return_value=[])
        for wrong in (200,403,500):
            page.request.get.side_effect=lambda url, status=wrong: (SimpleNamespace(status=200,json=lambda:{'items':[],'total':0})
                if url.endswith('/versions') else SimpleNamespace(status=status))
            with self.subTest(status=wrong),self.assertRaisesRegex(AssertionError,'exact 404'):
                owner.confirm_gone(1)
            self.assertEqual(len(state['remaining_resources']),2)
            self.assertNotIn('cleanup',state)
        page.request.get.side_effect=lambda url: (SimpleNamespace(status=200,json=lambda:{'items':[],'total':0})
            if url.endswith('/versions') else SimpleNamespace(status=404))
        owner.confirm_gone(1)
        self.assertEqual([r['id'] for r in state['remaining_resources']],[2])
        self.assertEqual(state['cleanup'][-1]['detail_status'],404)

    def test_partial_import_records_exact_declared_owned_id_before_failing(self):
        owner,state,page=self.owner();state['remaining_resources'].clear()
        rows=corpus('owned',10,'sglang','rocm')
        page.request.post.return_value=SimpleNamespace(status=500)
        owner.listed=Mock(return_value=[{'id':7,'name':'owned-003'},{'id':9,'name':'foreign'}])
        with self.assertRaisesRegex(AssertionError,'import failed'):owner.register(rows)
        self.assertEqual(state['remaining_resources'],[{'kind':'image-series','id':7,'name':'owned-003'}])
        self.assertEqual(state['pending_declared_names'],[r['name'] for r in rows])
        self.assertEqual(state['imports'][0]['body'],{'series':rows})


class MetadataPageCleanup(unittest.TestCase):
    def test_page_close_is_observed_and_failure_not_silently_green(self):
        from metadata_table_checks import close_probe
        page=Mock();page.is_closed.return_value=False
        state={'page_closed':False}
        with self.assertRaisesRegex(AssertionError,'did not close'):close_probe(page,state)
        self.assertFalse(state['page_closed'])
        self.assertIn('page_close_error',state)
        page.is_closed.return_value=True
        close_probe(page,state)
        self.assertTrue(state['page_closed'])

    def test_secondary_close_failure_preserves_primary_diagnosis(self):
        from metadata_table_checks import close_probe
        page=Mock();page.close.side_effect=RuntimeError('close held')
        state={'page_closed':False}
        close_probe(page,state,AssertionError('last-page identity wrong'))
        self.assertEqual(state['primary_failure_preserved'],'last-page identity wrong')
        self.assertEqual(state['page_close_error'],'close held')


class SeparatePageNavigation(unittest.TestCase):
    def setUp(self):
        self.expectation = patch('metadata_table_checks.expect', lambda locator: locator)
        self.expectation.start()
        self.addCleanup(self.expectation.stop)

    def test_initial_independent_page_navigates_before_awaiting_images_response(self):
        from metadata_table_checks import refresh
        from test_metadata_table_refresh_race import RecordedPage
        page=RecordedPage(overlaps=False)
        self.assertEqual(refresh(page,'http://owned', {'page': {'title': 'Images'}},initial=True),[{'id':2,'name':'new'}])
        self.assertEqual(page.goto_url,'http://owned/images')
        self.assertNotIn('reload',page.events)
        page=RecordedPage(overlaps=False)
        refresh(page,'http://owned', {'page': {'title': 'Images'}})
        self.assertIn('reload',page.events)
        self.assertIsNone(page.goto_url)

    def test_refresh_contract_does_not_accept_fabricated_server_paging_query(self):
        from metadata_table_checks import refresh
        from test_metadata_table_refresh_race import RecordedPage
        page=RecordedPage(overlaps=False,query='?page=1')
        with self.assertRaisesRegex(AssertionError,'real client-side list GET'):
            refresh(page,'http://owned', {'page': {'title': 'Images'}},initial=True)


class SelectedPageSizeOpening(unittest.TestCase):
    def test_selected_label_does_not_require_clicking_covered_input(self):
        import metadata_table_checks as browser
        page,pager,selector,shell=Mock(),Mock(),Mock(),Mock()
        pager.get_by_role.return_value=selector
        selector.locator.return_value=shell
        selector.get_attribute.return_value='owned-listbox'
        options=page.locator.return_value.locator.return_value.locator.return_value
        options.evaluate_all.return_value=[10,20,50,100]
        with patch.object(browser,'pagination',return_value=pager),patch.object(browser,'expect'):
            browser.choose_size(page,10)
        selector.click.assert_not_called()
        shell.click.assert_called_once_with()
        self.assertIn('ant-select-selector',selector.locator.call_args.args[0])
        options.nth.assert_called_once_with(0)
        options.nth.return_value.click.assert_called_once_with()
        options.filter.assert_not_called()

    def test_selection_index_comes_from_declared_order_not_english_labels(self):
        import metadata_table_checks as browser
        for index,size in enumerate((10,20,50,100)):
            page,pager,selector=Mock(),Mock(),Mock()
            pager.get_by_role.return_value=selector
            selector.get_attribute.return_value='owned-listbox'
            options=page.locator.return_value.locator.return_value.locator.return_value
            options.evaluate_all.return_value=[10,20,50,100]
            with patch.object(browser,'pagination',return_value=pager),patch.object(browser,'expect'):
                browser.choose_size(page,size)
            options.nth.assert_called_once_with(index)
            options.filter.assert_not_called()

    def test_page_size_universe_is_not_shrunk_to_rendered_subset(self):
        import metadata_table_checks as browser
        page,pager,selector=Mock(),Mock(),Mock()
        pager.get_by_role.return_value=selector
        selector.get_attribute.return_value='owned-listbox'
        options=page.locator.return_value.locator.return_value.locator.return_value
        options.evaluate_all.return_value=[10,20]
        with patch.object(browser,'pagination',return_value=pager),patch.object(browser,'expect'):
            with self.assertRaisesRegex(AssertionError,'all page-size options'):
                browser.choose_size(page,10)
        options.nth.assert_not_called()


class CrossPageDetailIdentity(unittest.TestCase):
    def test_200_for_wrong_independent_id_is_not_row_operation_proof(self):
        import contextlib
        import metadata_table_checks as browser
        page=Mock()
        response=SimpleNamespace(status=200,url='http://owned/api/v1/images/12',
            request=SimpleNamespace(method='GET',post_data=None),json=lambda:{'id':12,'name':'owned-029'})
        page.expect_response.return_value=contextlib.nullcontext(SimpleNamespace(value=response))
        page.request.get.return_value=SimpleNamespace(status=200,json=lambda:{'id':99,'name':'owned-029'})
        state={}
        with patch.object(browser,'expect'),self.assertRaisesRegex(AssertionError,'independent detail identity'):
            browser.row_detail(page,SimpleNamespace(base='http://owned'),{'seriesTable':{'details':'Details'}},
                'owned-029',12,30,3,state)
        page.request.get.assert_called_once_with('http://owned/api/v1/images/12')
        self.assertNotIn('cross_page_actions',state)

    def test_wrong_browser_row_identity_does_not_fall_back_to_independent_GET(self):
        import contextlib
        import metadata_table_checks as browser
        page=Mock()
        response=SimpleNamespace(status=200,url='http://owned/api/v1/images/12',
            request=SimpleNamespace(method='GET',post_data=None),json=lambda:{'id':99,'name':'foreign'})
        page.expect_response.return_value=contextlib.nullcontext(SimpleNamespace(value=response))
        with patch.object(browser,'expect'),self.assertRaisesRegex(AssertionError,'browser detail identity'):
            browser.row_detail(page,SimpleNamespace(base='http://owned'),{'seriesTable':{'details':'Details'}},
                'owned-029',12,30,3,{})
        page.request.get.assert_not_called()


class RestoreAfterScopedCleanup(unittest.TestCase):
    def test_empty_active_scope_requires_navigation_and_nonempty_custom_tab_before_size(self):
        import metadata_table_checks as browser
        baseline=[{'id':7,'source':'custom'},{'id':8,'source':'official'}]
        lifecycle={'filter_empty':True,'tab':'custom','size':10,'calls':[]}
        page=Mock()
        custom_tab=Mock()
        table=Mock()
        rows=table.locator.return_value
        rows.evaluate_all.side_effect=lambda script:[7] if lifecycle['tab']=='custom' and not lifecycle['filter_empty'] else []
        def roles(role,**kwargs):
            return custom_tab if role=='tab' else table
        page.get_by_role.side_effect=roles
        def select_custom():
            lifecycle['tab']='custom';lifecycle['calls'].append('custom')
        custom_tab.click.side_effect=select_custom
        def navigation(*args,**kwargs):
            lifecycle.update(filter_empty=False,tab='official')
            lifecycle['calls'].append('refresh')
            return copy.deepcopy(baseline)
        def size(*args):
            if lifecycle['filter_empty'] or lifecycle['tab']!='custom':
                raise AssertionError('Page Size absent for empty active-filter scope')
            lifecycle['size']=args[1];lifecycle['calls'].append('choose20')
        # This is an orchestration regression for the observed no-pager state,
        # not a claim that a fake table is browser evidence.
        with patch.object(browser,'refresh',side_effect=navigation),patch.object(browser,'choose_size',side_effect=size),              patch.object(browser,'expect'),patch.object(browser,'selected_page_size',create=True,side_effect=lambda p:lifecycle['size']):
            browser.restore_baseline_table(page,SimpleNamespace(base='http://owned'),
                {'page':{'tab':{'custom':'Custom'}}},baseline,{})
        self.assertEqual(lifecycle['calls'],['refresh','custom','choose20','refresh','custom'])
        self.assertEqual(lifecycle['size'],20)


    def test_same_visible_rows_do_not_hide_wrong_persisted_page_size(self):
        import metadata_table_checks as browser
        baseline=[{'id':7,'source':'custom'}]
        page=Mock()
        page.get_by_role.return_value.locator.return_value.evaluate_all.return_value=[7]
        state={}
        with patch.object(browser,'refresh',return_value=copy.deepcopy(baseline)),              patch.object(browser,'choose_size'),patch.object(browser,'expect'),              patch.object(browser,'selected_page_size',return_value=10):
            with self.assertRaisesRegex(AssertionError,'page-size20 persists'):
                browser.restore_baseline_table(page,SimpleNamespace(base='http://owned'),
                    {'page':{'tab':{'custom':'Custom'}}},baseline,state)
        self.assertNotIn('baseline_restoration',state)

    def test_missing_original_baseline_row_cannot_be_hidden_by_preference_restore(self):
        import metadata_table_checks as browser
        page=Mock()
        with patch.object(browser,'refresh',return_value=[]),patch.object(browser,'choose_size') as choose:
            with self.assertRaisesRegex(AssertionError,'full baseline GET before'):
                browser.restore_baseline_table(page,SimpleNamespace(base='http://owned'),
                    {'page':{'tab':{'custom':'Custom'}}},[{'id':7,'source':'custom'}],{})
        choose.assert_not_called()


if __name__=='__main__':unittest.main()
