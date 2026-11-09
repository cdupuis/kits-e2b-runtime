#!/usr/bin/env python3
"""Adapter transport/input regressions; the official TCK observes real VMs."""
import importlib.util
import io
import json
import pathlib
import tempfile
import unittest
import urllib.error
from unittest import mock

spec = importlib.util.spec_from_file_location(
    'native_tck', pathlib.Path(__file__).with_name('native-tck.py'))
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


class AdapterTests(unittest.TestCase):
    def test_overrides_preserve_empty_and_literal_values(self):
        literal = 'spaces = $HOME; `printf injected` $(printf injected)'
        refs, flags = adapter.options([
            '--arg', 'empty=', '--arg', 'text=' + literal,
            '--env', 'TEXT=' + literal, '--env', 'EMPTY=', 'fixture'])
        self.assertEqual(refs, ['fixture'])
        self.assertEqual(flags['args'], {'empty': '', 'text': literal})
        self.assertEqual(flags['env'], {'TEXT': literal, 'EMPTY': ''})

    def test_invalid_override_is_an_error(self):
        for args in [['--arg'], ['--arg', 'missing_equals'], ['--env', '=value']]:
            with self.subTest(args=args), self.assertRaises(ValueError):
                adapter.options(args)

    def test_http_failures_are_never_policy_refusals(self):
        with tempfile.TemporaryDirectory() as directory:
            key = pathlib.Path(directory) / 'key'
            key.write_text('synthetic-test-key')
            for status in [400, 403, 404, 500]:
                error = urllib.error.HTTPError('http://test', status, 'failure', {},
                                               io.BytesIO(b'transport failure'))
                with self.subTest(status=status), mock.patch.dict(
                        adapter.os.environ, {'NATIVE_TCK_API_KEY_FILE': str(key)}), \
                        mock.patch.object(adapter.urllib.request, 'urlopen', side_effect=error):
                    with self.assertRaisesRegex(RuntimeError, f'HTTP {status}: transport failure'):
                        adapter.api('POST', '/sandboxes', {})

    def test_incompatible_recreation_does_not_stop_or_rewrite_instance(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            state = {'sandboxID': 'original', 'source': {'references': ['fixture'],
                     'args': {}, 'env': {}}, 'stopped': False}
            path = root / 'instance.json'
            path.write_text(json.dumps(state))
            def helper(*args, **kwargs):
                if args[0] == 'types':
                    return '["volume"]'
                if args[0] == 'plan':
                    return '{"storage": [{"volume": {"path": "/data"}}]}'
                raise adapter.Refused('incompatible retained volume')
            with mock.patch.dict(adapter.os.environ, {
                    'NATIVE_TCK_CLAIMS': 'volume', 'NATIVE_TCK_STATE': directory,
                    'NATIVE_TCK_TEAM_ID': 'team'}), \
                    mock.patch.object(adapter, 'helper', side_effect=helper), \
                    mock.patch.object(adapter, 'api') as api:
                with self.assertRaises(adapter.Refused):
                    adapter.main(['recreate', 'instance', '--arg', 'size=2g'])
                api.assert_not_called()
                self.assertEqual(json.loads(path.read_text()), state)

    def test_removed_volume_observation_queries_backing_store(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / 'instance.removed.json').write_text('{"sandboxID": "original"}')
            with mock.patch.dict(adapter.os.environ, {
                    'NATIVE_TCK_CLAIMS': 'volume', 'NATIVE_TCK_STATE': directory,
                    'NATIVE_TCK_TEAM_ID': 'team'}), \
                    mock.patch.object(adapter, 'helper', side_effect=['["volume"]', '["/orphan"]']) as helper, \
                    mock.patch('sys.stdout', new_callable=io.StringIO) as output:
                self.assertEqual(adapter.main(['volume-paths', 'instance']), 0)
                helper.assert_called_with('volume-paths', 'team', 'original')
                self.assertEqual(json.loads(output.getvalue()), ['/orphan'])

    def test_host_observations_have_no_instance_alias_or_api_calls(self):
        for argv in [
                ['host-mounts', 'fixture'],
                ['host-mount-read', 'handle', 'dir/marker'],
                ['host-mount-rm', 'handle']]:
            with self.subTest(argv=argv), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                catalog = root / 'catalog.json'
                catalog.write_text('{"host-mount": "fixture"}')
                with mock.patch.dict(adapter.os.environ, {
                        'NATIVE_TCK_CLAIMS': 'host-mount', 'NATIVE_TCK_STATE': directory,
                        'NATIVE_TCK_CATALOG': str(catalog), 'NATIVE_TCK_TEAM_ID': 'team'}), \
                        mock.patch.object(adapter, 'helper', side_effect=['["host-mount"]', 'observed']) as helper, \
                        mock.patch.object(adapter, 'api') as api, \
                        mock.patch('sys.stdout', new_callable=io.StringIO) as output:
                    self.assertEqual(adapter.main(argv), 0)
                    helper.assert_called_with(argv[0], 'team', *argv[1:])
                    self.assertEqual(output.getvalue(), 'observed')
                    api.assert_not_called()

    def test_host_observation_failures_stay_errors(self):
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.dict(adapter.os.environ, {
                    'NATIVE_TCK_CLAIMS': 'host-mount', 'NATIVE_TCK_STATE': directory,
                    'NATIVE_TCK_TEAM_ID': 'team'}), \
                    mock.patch.object(adapter, 'helper', side_effect=[
                        '["host-mount"]', RuntimeError('registry is unreadable')]), \
                    mock.patch.object(adapter, 'api') as api:
                with self.assertRaisesRegex(RuntimeError, 'registry is unreadable'):
                    adapter.main(['host-mount-read', 'handle', 'marker'])
                api.assert_not_called()

    def test_replacement_references_reset_kit_arguments(self):
        for flags, expected in [
                (['workload'], {}),
                (['workload', '--arg', 'new=value'], {'new': 'value'}),
                (['--arg', 'volume_size=2g'], {'volume_size': '2g'})]:
            with self.subTest(flags=flags), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                state = {'sandboxID': 'original', 'source': {
                    'references': ['workload', 'volume'],
                    'args': {'volume_size': '1g'}, 'env': {}}}
                (root / 'instance.json').write_text(json.dumps(state))
                catalog = root / 'catalog.json'
                catalog.write_text('{"workload": "workload", "volume": "volume"}')

                def helper(verb, **kwargs):
                    if verb == 'types':
                        return '["volume"]'
                    self.assertEqual(verb, 'plan')
                    self.assertEqual(json.loads(kwargs['data'])['args'], expected)
                    raise RuntimeError('plan observed')

                with mock.patch.dict(adapter.os.environ, {
                        'NATIVE_TCK_CLAIMS': 'volume', 'NATIVE_TCK_STATE': directory,
                        'NATIVE_TCK_CATALOG': str(catalog)}), \
                        mock.patch.object(adapter, 'helper', side_effect=helper), \
                        mock.patch.object(adapter, 'api') as api:
                    with self.assertRaisesRegex(RuntimeError, 'plan observed'):
                        adapter.main(['recreate', 'instance', *flags])
                    api.assert_not_called()
                    self.assertEqual(json.loads((root / 'instance.json').read_text()), state)

    def test_future_policy_preserves_current_selection_and_template(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            state = {'sandboxID': 'original', 'source': {'references': ['fixture']},
                     'selection': {'surface': 'original'}, 'futureReject': []}
            path = root / 'instance.json'
            path.write_text(json.dumps(state))
            with mock.patch.dict(adapter.os.environ, {
                    'NATIVE_TCK_CLAIMS': 'volume', 'NATIVE_TCK_STATE': directory}), \
                    mock.patch.object(adapter, 'helper', return_value='["volume"]'), \
                    mock.patch.object(adapter, 'api') as api:
                self.assertEqual(adapter.main([
                    'selection-policy', 'instance', '--reject-capability', 'volume']), 0)
                after = json.loads(path.read_text())
                self.assertEqual(after['selection'], state['selection'])
                self.assertEqual(after['source'], state['source'])
                self.assertEqual(after['futureReject'], ['volume'])
                api.assert_not_called()


if __name__ == '__main__':
    unittest.main()
