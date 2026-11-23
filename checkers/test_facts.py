"""Synthetic controls for the shared structural and lease predicates."""
import importlib.util
from pathlib import Path
import unittest

ROOT = Path(__file__).parent
spec = importlib.util.spec_from_file_location('facts', ROOT / 'facts.py')
facts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(facts)


def intent(seq=0, block=0):
    return f'I\t{seq}\t{block}\t' + 'a' * 64 + '\t10\t1\t100\n'


def ack(seq=0, block=0):
    return f'A\t{seq}\t{block}\t' + 'a' * 64 + '\t10\t11\t2\t101\n'


class Facts(unittest.TestCase):
    def test_exact_and_unresolved_intent(self):
        result = facts.read_ledger((intent() + ack() + intent(1, 1)).encode(), 'FINAL')
        self.assertEqual(result['metrics'], {'acked': 1, 'exact': 1, 'chainbreaks': 0, 'gaps': 0})
        self.assertEqual(result['records'][1]['kind'], 'I')

    def test_duplicate_block_rejected(self):
        with self.assertRaisesRegex(ValueError, 'block .* written twice'):
            facts.read_ledger((intent() + ack() + intent(1, 0)).encode(), 'FINAL')

    def test_orphan_ack_rejected(self):
        with self.assertRaisesRegex(ValueError, 'matching intent'):
            facts.read_ledger(ack().encode(), 'FINAL')

    def test_partial_tail_live_only(self):
        raw = (intent() + ack() + 'I\t1').encode()
        self.assertEqual(facts.read_ledger(raw, 'LIVE')['snapshot'], (intent() + ack()).encode())
        with self.assertRaisesRegex(ValueError, 'unterminated'):
            facts.read_ledger(raw, 'FINAL')
        with self.assertRaises(ValueError):
            facts.read_ledger(raw, 'OTHER')

    def test_schema_and_duplicate_terminal_rejected(self):
        for raw in [intent() + ack() + ack(), intent().replace('a' * 64, 'bad'), 'A\t0\n']:
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                facts.read_ledger(raw.encode(), 'FINAL')

    def test_ack_boundaries(self):
        self.assertEqual(facts.ack_window({'iwall': 99, 'wall': 105}, 100, 5), 'inside')
        self.assertEqual(facts.ack_window({'iwall': 100, 'wall': 100}, 100, 5), 'requested_after_U')
        self.assertEqual(facts.ack_window({'iwall': 99, 'wall': 106}, 100, 5), 'completed_after_drain')

    def test_lease_bound_and_missing_evidence(self):
        journal = 'workspace lease: fenced volume=v replica=r epoch=1: lease expired at 2026-01-01T00:00:00Z without renewal\n'
        self.assertEqual(facts.lease_bound(journal)[0], 1767225600000000000)
        self.assertIsNotNone(facts.lease_bound('')[2])
        self.assertIsNotNone(facts.lease_bound(journal.replace('2026-01-01T00:00:00Z', 'bad'))[2])

    def test_each_record_schema(self):
        error = 'E\t0\t0\t5\t2\t101\n'
        self.assertEqual(facts.read_ledger((intent() + error).encode(), 'FINAL')['records'][0]['errno'], 5)
        for kind, schema in facts.RECORD_SCHEMA.items():
            row = {'I': intent(), 'A': ack(), 'E': error}[kind]
            for position in schema['numeric'] + schema['lsn']:
                fields = row.rstrip('\n').split('\t')
                fields[position] = 'bad'
                prefix = '' if kind == 'I' else intent()
                with self.subTest(kind=kind, position=position), self.assertRaises(ValueError):
                    facts.read_ledger((prefix + '\t'.join(fields) + '\n').encode(), 'FINAL')

    def test_first_fence_uses_latest_renewal_bound(self):
        journal = (
            'workspace lease: renewal failed after 1s (lease valid until 2026-01-01T00:00:00Z): timeout\n'
            'workspace lease: renewal failed after 1s (lease valid until 2026-01-01T00:00:01Z): timeout\n'
            'workspace lease: fenced volume=v replica=r epoch=1: lease expired after a failed renewal\n'
            'workspace lease: fenced volume=v replica=r epoch=1: later diagnostic\n'
        )
        bound, cause, error = facts.lease_bound(journal)
        self.assertEqual(bound, facts.ts('2026-01-01T00:00:01Z'))
        self.assertEqual(cause, 'lease expired after a failed renewal')
        self.assertIsNone(error)


class Mutants(unittest.TestCase):
    def mutant(self, old, new):
        import types
        source = (ROOT / 'facts.py').read_text()
        self.assertEqual(source.count(old), 1)
        module = types.ModuleType('mutant')
        exec(compile(source.replace(old, new), '<synthetic-mutant>', 'exec'), module.__dict__)
        return module

    def test_read_ledger_duplicate_block_mutant(self):
        m = self.mutant('if block in blocks:', 'if False:')
        # The ordinary control rejects this; this mutant wrongly returns data.
        self.assertEqual(len(m.read_ledger((intent() + ack() + intent(1, 0)).encode(), 'FINAL')['records']), 2)

    def test_read_ledger_orphan_ack_mutant(self):
        m = self.mutant("r = recs.get(seq)", "r = recs.setdefault(seq, {'seq': seq, 'block': block, 'sha': p[3], 'kind': 'I', 'iwall': 0})")
        self.assertEqual(m.read_ledger(ack().encode(), 'FINAL')['metrics']['acked'], 1)

    def test_ack_window_mutant(self):
        m = self.mutant("if r['iwall'] >= U:", "if r['iwall'] > U:")
        self.assertEqual(m.ack_window({'iwall': 100, 'wall': 100}, 100, 5), 'inside')

    def test_lease_bound_mutant(self):
        m = self.mutant("return (None, None, 'no fence line: the old owner never fenced')", "return (0, 'invented', None)")
        self.assertIsNone(m.lease_bound('')[2])


if __name__ == '__main__':
    unittest.main()
