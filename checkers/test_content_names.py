import hashlib
import unittest
import types
from pathlib import Path

from oss_kernel import entries_match_names


def content_fixture():
    files = {}
    for prefix, data in [
        ('v/pool/0/0-', b'chunk-data'),
        ('v/seg/0-', b'segment-data'),
        ('v/root/', b'root-data'),
    ]:
        name = prefix + hashlib.sha256(data).hexdigest()
        files[name] = {'producer_exit': 0, 'exists': True, 'size': len(data), 'content': data}
    return files


def check_content(judge, files):
    return judge(
        'v',
        lambda prefix: [name for name in files if name.startswith(prefix)],
        lambda name: files[name],
    )


class ContentNames(unittest.TestCase):
    def test_corrupt_or_short_content_fails_even_with_forged_hash_label(self):
        for short in (False, True):
            files = content_fixture()
            name = next(iter(files))
            record = files[name]
            record['sha256'] = name.rsplit('-', 1)[-1]
            record['content'] = b'x' * (record['size'] - int(short))
            self.assertEqual(check_content(entries_match_names, files)['verdict'], 'FAIL')

    def test_invalid_inventory_is_unknown_and_short_hash_name_fails(self):
        for names in ([None], ['other/root/hash'], ['v/pool/../hash']):
            with self.subTest(names=names):
                result = entries_match_names('v', lambda _: names, lambda _: None)
                self.assertEqual(result['verdict'], 'UNKNOWN')
        files = content_fixture()
        name = next(iter(files))
        files[name[:-1]] = files.pop(name)
        self.assertEqual(check_content(entries_match_names, files)['verdict'], 'FAIL')

    def test_duplicate_inventory_and_reader_exception_are_unknown(self):
        files = content_fixture()
        name = next(iter(files))
        self.assertEqual(
            entries_match_names('v', lambda _: [name, name], lambda key: files[key])['verdict'],
            'UNKNOWN',
        )

        def unreadable(_):
            raise OSError('read failed')

        self.assertEqual(entries_match_names('v', unreadable, unreadable)['verdict'], 'UNKNOWN')

    def test_independent_corruption_survives_unknown_namespace(self):
        files = content_fixture()
        next(iter(files.values()))['content'] = b'corrupt'
        result = entries_match_names(
            'v',
            lambda prefix: (
                None if '/root/' in prefix else [n for n in files if n.startswith(prefix)]
            ),
            lambda name: files[name],
        )
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertTrue(result['unknown'])

    def test_every_publish_prefix_is_checked_including_interrupted_content(self):
        files = content_fixture()
        published = {}
        for name, record in files.items():
            self.assertEqual(check_content(entries_match_names, published)['verdict'], 'PASS')
            published[name] = dict(record)
            self.assertEqual(check_content(entries_match_names, published)['verdict'], 'PASS')
            published[name]['content'] = record['content'][:-1]
            self.assertEqual(check_content(entries_match_names, published)['verdict'], 'FAIL')
            published[name] = record

    def test_hash_and_complete_read_mutants(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        for anchor in (
            'if hashlib.sha256(data).hexdigest() != expected_sha:',
            'if len(data) != size:',
        ):
            self.assertEqual(source.count(anchor), 1)
            module = types.ModuleType('content_mutant')
            exec(compile(source.replace(anchor, 'if False:'), '<mutant>', 'exec'), module.__dict__)
            files = content_fixture()
            record = next(iter(files.values()))
            if 'hashlib' in anchor:
                record['content'] = b'x' * record['size']
            else:
                record['size'] += 1
            self.assertEqual(check_content(entries_match_names, files)['verdict'], 'FAIL')
            self.assertEqual(check_content(module.entries_match_names, files)['verdict'], 'PASS')

    def test_complete_pool_segment_and_root_contents_match_their_names(self):
        files = content_fixture()
        result = check_content(entries_match_names, files)
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(set(result['checked']), set(files))

    def test_failed_or_incomplete_filer_observation_is_unknown(self):
        for field, value in [
            ('producer_exit', 1),
            ('exists', False),
            ('size', None),
            ('content', None),
        ]:
            with self.subTest(field=field):
                files = content_fixture()
                next(iter(files.values()))[field] = value
                self.assertEqual(check_content(entries_match_names, files)['verdict'], 'UNKNOWN')
        files = content_fixture()
        self.assertEqual(
            entries_match_names('v', lambda _: None, lambda name: files[name])['verdict'], 'UNKNOWN'
        )


if __name__ == '__main__':
    unittest.main()
