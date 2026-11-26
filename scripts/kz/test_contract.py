from pathlib import Path
import unittest
from contract import check

RECIPE = '''
fss = []string{"ext4", "xfs"}
for _, fs := range fss {
    for _, kind := range []string{"restore"} {
        tb := env.hosts["kz"+fs+kind[:1]]
        ho.Targets = append(ho.Targets, kzHandoffTarget{Kind: kind, Volume: tb.volume})
    }
}
'''


class ContractTest(unittest.TestCase):
    def test_product_target_recipe_and_runner_cannot_drift(self):
        here = Path(__file__).parent
        host, judge = (here/"host.py").read_text(), (here/"judge.py").read_text()
        self.assertEqual(check(RECIPE, host, judge), ("kzext4r", "kzxfsr"))
        for product, h, j in (
            (RECIPE.replace('"restore"', '"restore", "fork"'), host, judge),
            (RECIPE.replace('"ext4", "xfs"', '"ext4"'), host, judge),
            (RECIPE.replace('kind[:1]', '"r"'), host, judge),
            (RECIPE.replace('ho.Targets =', 'other.Targets ='), host, judge),
            (RECIPE, host.replace('TARGET_COUNT = 2', 'TARGET_COUNT = 4'), judge),
            (RECIPE, host, judge.replace('"kzxfsr"', '"kzxfsf"')),
        ):
            with self.subTest(product=product, host=h, judge=j):
                with self.assertRaises(ValueError): check(product, h, j)
