"""Synthetic controls for classification and exact restored images."""
import hashlib
import io
from pathlib import Path
import types
import unittest
import facts


def record(seq=0, block=0, kind='A', h0=10, h1=11, payload=b'abcd'):
    return {'seq': seq, 'block': block, 'kind': kind, 'h0': h0, 'h1': h1,
            'sha': hashlib.sha256(payload).hexdigest()}


class Classification(unittest.TestCase):
    def test_boundary_and_incomplete_evidence(self):
        cases = [(record(), 11, 'must'), (record(), 10, 'lost'),
                 (record(kind='I'), 11, 'either'), (record(kind='E'), 11, 'either'),
                 (record(h0=None), 11, 'lsnless'), (record(h1=None), 11, 'lsnless'),
                 (record(h1=13), 11, 'lsnless')]
        for value, boundary, expected in cases:
            with self.subTest(value=value, boundary=boundary):
                self.assertEqual(facts.classify_write(value, boundary), expected)


class Images(unittest.TestCase):
    def test_required_write_must_be_present_byte_exact(self):
        recs = {0: record()}
        for data, verdict in [(b'abcd', 'PASS'), (bytes(4), 'FAIL'), (b'xxxx', 'FAIL')]:
            with self.subTest(data=data):
                result = facts.image_exact(recs, io.BytesIO(data), 11, 1, 4)
                self.assertEqual(result['verdict'], verdict)

    def test_lost_write_must_be_absent(self):
        for data, verdict in [(bytes(4), 'PASS'), (b'abcd', 'FAIL')]:
            with self.subTest(data=data):
                self.assertEqual(facts.image_exact({0: record()}, io.BytesIO(data), 10, 1, 4)['verdict'], verdict)

    def test_uncertain_write_is_not_an_exact_ack(self):
        for kind, h1 in [('A', None), ('I', 11), ('E', 11)]:
            for data in (bytes(4), b'abcd'):
                with self.subTest(kind=kind, data=data):
                    result = facts.image_exact({0: record(kind=kind, h1=h1)}, io.BytesIO(data), 11, 1, 4)
                    self.assertEqual(result['verdict'], 'NOT_COUNTED')
            result = facts.image_exact({0: record(kind=kind, h1=h1)}, io.BytesIO(b'xxxx'), 11, 1, 4)
            self.assertEqual(result['verdict'], 'FAIL')

    def test_unwritten_and_geometry_checks(self):
        recs = {0: record()}
        for data, blocks in [(b'abcdx', 1), (b'abc', 1), (b'abcdxxxx', 2)]:
            with self.subTest(data=data, blocks=blocks):
                self.assertEqual(facts.image_exact(recs, io.BytesIO(data), 11, blocks, 4)['verdict'], 'FAIL')
        outside = {0: record(block=1)}
        self.assertEqual(facts.image_exact(outside, io.BytesIO(bytes(4)), 11, 1, 4)['verdict'], 'FAIL')
        self.assertEqual(facts.image_exact(recs, io.BytesIO(b'abcd' + bytes(4)), 11, 2, 4)['verdict'], 'PASS')

    def test_raced_write_requires_explicit_matching_decision(self):
        recs = {0: record(h1=None)}
        cases = [('none', 11, b'abcd', 'NOT_COUNTED'), ('1', 11, b'abcd', 'FAIL'),
                 ('0', 11, b'abcd', 'PASS'), ('0', 11, bytes(4), 'FAIL'),
                 ('0', 10, bytes(4), 'PASS'), ('0', 10, b'abcd', 'FAIL')]
        for decision, boundary, data, verdict in cases:
            with self.subTest(decision=decision, boundary=boundary, data=data):
                result = facts.image_exact(recs, io.BytesIO(data), boundary, 1, 4, raced=decision)
                self.assertEqual(result['verdict'], verdict)

    def test_only_last_ack_can_use_raced_exception(self):
        recs = {0: record(h1=None), 1: record(seq=1, block=1, h0=11, h1=None, payload=b'efgh')}
        data = b'abcdefgh'
        self.assertEqual(facts.image_exact(recs, io.BytesIO(data), 12, 2, 4, raced='0')['verdict'], 'FAIL')
        self.assertEqual(facts.image_exact(recs, io.BytesIO(data), 12, 2, 4, raced='1')['verdict'], 'NOT_COUNTED')

    def test_failed_write_content_is_allowed_but_not_required(self):
        recs = {0: record(), 1: record(seq=1, block=1, kind='E', payload=b'efgh')}
        for tail in (bytes(4), b'efgh'):
            self.assertEqual(facts.image_exact(recs, io.BytesIO(b'abcd' + tail), 11, 2, 4)['verdict'], 'PASS')

    def test_invalid_input_never_returns_pass(self):
        class Unreadable:
            def read(self, length):
                raise OSError('synthetic evidence read failure')
        with self.assertRaises(OSError):
            facts.image_exact({0: record()}, Unreadable(), 11, 1, 4)
        for boundary, blocks, size in [(-1, 1, 4), (11, 0, 4), (11, 1, 0)]:
            with self.subTest(boundary=boundary, blocks=blocks, size=size), self.assertRaises(ValueError):
                facts.image_exact({}, io.BytesIO(), boundary, blocks, size)
        self.assertEqual(facts.image_exact({}, io.BytesIO(bytes(4)), 11, 1, 4)['verdict'], 'NOT_COUNTED')


class ImageMutants(unittest.TestCase):
    def mutant(self, old, new):
        source = Path(facts.__file__).read_text()
        self.assertEqual(source.count(old), 1)
        module = types.ModuleType('image_mutant')
        exec(compile(source.replace(old, new), '<synthetic-image-mutant>', 'exec'), module.__dict__)
        return module

    def test_classifier_must_not_treat_inexact_ack_as_exact(self):
        mutant = self.mutant(' or h1 != h0 + 1', '')
        value = record(h1=13)
        self.assertEqual(facts.classify_write(value, 13), 'lsnless')
        self.assertEqual(mutant.classify_write(value, 13), 'must')

    def test_image_must_not_ignore_missing_required_bytes(self):
        mutant = self.mutant("if category == 'must' and digest != record['sha']:", 'if False:')
        recs = {0: record()}
        self.assertEqual(facts.image_exact(recs, io.BytesIO(bytes(4)), 11, 1, 4)['verdict'], 'FAIL')
        self.assertEqual(mutant.image_exact(recs, io.BytesIO(bytes(4)), 11, 1, 4)['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
