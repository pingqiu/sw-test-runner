import hashlib
import unittest
import types
from pathlib import Path

from facts import acknowledged_writes_survive


def survival_fixture():
    binding = {'run': 'synthetic', 'volume': 'v', 'crash_id': 'crash-1'}
    scope = {
        'binding': binding,
        'host': 'writer',
        'witness_host': 'observer',
        'blocks': 4,
        'block_bytes': 8,
    }
    data = b'record00'
    sha = hashlib.sha256(data).hexdigest()
    ledger = (f'I\t0\t0\t{sha}\tNA\t1\t1\nA\t0\t0\t{sha}\tNA\tNA\t2\t2\n').encode()
    witness = {
        'binding': dict(binding),
        'host': 'observer',
        'producer_exit': 0,
        'source': 'off_host_guest_ack',
        'durability': 'fsync',
        'ledger': ledger,
    }
    crash = {
        'binding': dict(binding),
        'host': 'writer',
        'producer_exit': 0,
        'source': 'external_controller',
        'kind': 'power_cut',
        'confirmed_dead': True,
    }
    blocks = {
        0: {
            'binding': dict(binding),
            'source': 'recovered_guest',
            'producer_exit': 0,
            'recovery_complete': True,
            'guest_writes': 0,
            'exists': True,
            'block': 0,
            'data': data,
        }
    }
    return scope, witness, crash, blocks


def check_survival(judge, fixture):
    scope, witness, crash, blocks = fixture
    return judge(scope, witness, crash, lambda block: blocks.get(block))


class AckSurvival(unittest.TestCase):
    def test_fua_with_external_vm_kill_is_supported(self):
        fixture = survival_fixture()
        fixture[1]['durability'] = 'fua'
        fixture[2]['kind'] = 'vm_kill'
        self.assertEqual(check_survival(acknowledged_writes_survive, fixture)['verdict'], 'PASS')

    def test_missing_block_or_reader_error_is_not_definite_data_loss(self):
        fixture = survival_fixture()
        fixture[3].clear()
        self.assertEqual(check_survival(acknowledged_writes_survive, fixture)['verdict'], 'UNKNOWN')
        def failed_reader(_):
            raise OSError('read failed')
        self.assertEqual(acknowledged_writes_survive(*fixture[:3], failed_reader)['verdict'], 'UNKNOWN')

    def test_host_local_probe_or_process_kill_cannot_qualify(self):
        for target, field, value in [
            (0, 'witness_host', 'writer'),
            (1, 'source', 'in_process_sync'),
            (1, 'producer_exit', True),
            (1, 'durability', 'write'),
            (2, 'kind', 'sigkill'),
            (2, 'confirmed_dead', False),
            (2, 'source', 'local_log'),
            (2, 'binding', {'run': 'other'}),
        ]:
            with self.subTest(field=field, value=value):
                fixture = survival_fixture()
                fixture[target][field] = value
                self.assertEqual(
                    check_survival(acknowledged_writes_survive, fixture)['verdict'], 'UNKNOWN'
                )

    def test_empty_malformed_or_partial_witness_never_passes(self):
        for ledger in (b'', b'I\t0', b'A\t0\t0\tbad\n'):
            fixture = survival_fixture()
            fixture[1]['ledger'] = ledger
            self.assertEqual(
                check_survival(acknowledged_writes_survive, fixture)['verdict'], 'UNKNOWN'
            )

    def test_missing_failed_or_rewritten_recovery_evidence_is_unknown(self):
        for field, value in [
            ('binding', {}),
            ('producer_exit', 1),
            ('data', b''),
            ('recovery_complete', False),
            ('guest_writes', 1),
            ('source', 'log_file'),
            ('block', 1),
        ]:
            with self.subTest(field=field):
                fixture = survival_fixture()
                fixture[3][0][field] = value
                self.assertEqual(
                    check_survival(acknowledged_writes_survive, fixture)['verdict'], 'UNKNOWN'
                )

    def test_external_ack_survives_power_loss_even_without_lsn(self):
        self.assertEqual(
            check_survival(acknowledged_writes_survive, survival_fixture())['verdict'], 'PASS'
        )

    def test_acknowledged_bytes_lost_after_recovery_fail(self):
        fixture = survival_fixture()
        fixture[3][0]['data'] = b'wrong000'
        result = check_survival(acknowledged_writes_survive, fixture)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['violations'][0]['label'], 'ACKED-LOST')

    def test_all_witnessed_acks_are_checked_and_independent_loss_survives_unknown(self):
        fixture = survival_fixture()
        sha = hashlib.sha256(b'record01').hexdigest()
        fixture[1]['ledger'] += (
            f'I\t1\t1\t{sha}\tNA\t3\t3\nA\t1\t1\t{sha}\tNA\tNA\t4\t4\n'
        ).encode()
        fixture[3][1] = dict(fixture[3][0], block=1, data=b'record01')
        self.assertEqual(check_survival(acknowledged_writes_survive, fixture)['checked'], [0, 1])
        fixture[3][1]['exists'] = False
        fixture[3][0]['producer_exit'] = 1
        result = check_survival(acknowledged_writes_survive, fixture)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['violations'][0]['seq'], 1)
        self.assertTrue(result['unknown'])

    def test_comparison_and_off_host_custody_mutants_are_caught(self):
        source = (Path(__file__).parent / 'facts.py').read_text()
        for anchor, replacement, corrupt in [
            ("intact = hashlib.sha256(data).hexdigest() == record['sha']", 'intact = True', True),
            ("or scope['host'] == scope['witness_host']", 'or False', False),
        ]:
            self.assertEqual(source.count(anchor), 1)
            module = types.ModuleType('survival_mutant')
            exec(compile(source.replace(anchor, replacement), '<mutant>', 'exec'), module.__dict__)
            fixture = survival_fixture()
            if corrupt:
                fixture[3][0]['data'] = b'wrong000'
            else:
                fixture[0]['host'] = fixture[0]['witness_host']
                fixture[2]['host'] = fixture[0]['host']
            self.assertNotEqual(
                check_survival(acknowledged_writes_survive, fixture)['verdict'], 'PASS'
            )
            self.assertEqual(
                check_survival(module.acknowledged_writes_survive, fixture)['verdict'], 'PASS'
            )


if __name__ == '__main__':
    unittest.main()
