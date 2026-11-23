import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch
from unit_properties import show_unit_properties


PROPERTIES = (
    'LoadState=loaded\nActiveState=inactive\nSubState=dead\n'
    'Result=success\nExecMainStatus=0\nInvocationID=recorded\n')


class UnitProperties(unittest.TestCase):
    def test_failed_producer_never_accepts_valid_looking_output(self):
        reply = subprocess.CompletedProcess([], 1, PROPERTIES, 'bus unavailable')
        with patch('subprocess.run', return_value=reply):
            with self.assertRaisesRegex(ValueError, 'systemctl show exit 1: bus unavailable'):
                show_unit_properties('sample.service', 2, 'systemctl')

    def test_complete_observation_preserves_values_without_deciding_outcome(self):
        reply = subprocess.CompletedProcess([], 0, PROPERTIES.replace('Result=success', 'Result=exit-code'), '')
        with patch('subprocess.run', return_value=reply) as producer:
            result = show_unit_properties('sample.service', 1.25, 'custom-systemctl')
        self.assertEqual(result['Result'], 'exit-code')
        self.assertEqual(result['InvocationID'], 'recorded')
        args, kwargs = producer.call_args
        self.assertEqual(args[0][0], 'custom-systemctl')
        self.assertEqual(args[0][-2:], ['--', 'sample.service'])
        self.assertEqual(kwargs['timeout'], 1.25)

    def test_missing_service_property_is_unknown(self):
        fields = PROPERTIES.splitlines()
        for missing in [field for field in fields if not field.startswith("ExecMainStatus=")]:
            with self.subTest(missing=missing):
                text = '\n'.join(line for line in fields if line != missing) + '\n'
                reply = subprocess.CompletedProcess([], 0, text, '')
                with patch('subprocess.run', return_value=reply):
                    with self.assertRaisesRegex(ValueError, 'not parsable'):
                        show_unit_properties('sample.service', 2)

    def test_missing_main_status_keeps_existing_unknown_status_marker(self):
        text = PROPERTIES.replace('ExecMainStatus=0\n', '')
        with patch('subprocess.run', return_value=subprocess.CompletedProcess([], 0, text, '')):
            result = show_unit_properties('sample.service', 2)
        self.assertEqual(result['ExecMainStatus'], '-')

    def test_scope_does_not_require_main_process_status(self):
        text = PROPERTIES.replace('ExecMainStatus=0\n', '')
        with patch('subprocess.run', return_value=subprocess.CompletedProcess([], 0, text, '')):
            result = show_unit_properties('sample.scope', 2)
        self.assertEqual(result['ExecMainStatus'], '-')

    def test_empty_or_malformed_output_is_unknown(self):
        for text in ['', 'garbage', PROPERTIES.replace('ActiveState=inactive', 'ActiveState=')]:
            with self.subTest(text=text):
                with patch('subprocess.run', return_value=subprocess.CompletedProcess([], 0, text, '')):
                    with self.assertRaisesRegex(ValueError, 'not parsable'):
                        show_unit_properties('sample.service', 2)

    def test_timeout_is_unknown(self):
        with patch('subprocess.run', side_effect=subprocess.TimeoutExpired('systemctl', 2)):
            with self.assertRaisesRegex(ValueError, 'timed out'):
                show_unit_properties('sample.service', 2)

    def test_failed_producer_mutant_is_caught(self):
        source = Path(__file__).with_name('unit_properties.py').read_text()
        condition = 'if r.returncode != 0:'
        self.assertEqual(source.count(condition), 1)
        namespace = {}
        exec(compile(source.replace(condition, 'if False:'), '<status-mutant>', 'exec'), namespace)
        reply = subprocess.CompletedProcess([], 1, PROPERTIES, 'bus unavailable')
        with patch('subprocess.run', return_value=reply):
            # The mutant accepts a producer failure as a normal observation.
            self.assertEqual(namespace['show_unit_properties']('sample.service', 2)['ActiveState'], 'inactive')


if __name__ == '__main__':
    unittest.main()
