# Scenario Syntax

A scenario is one YAML file: identity, optional cluster/topology/targets, then an
ordered list of **phases**, each a list of **actions**. This is the quick
reference; the full field spec is [Scenario Spec](../scenario-spec.md).

## Annotated example

```yaml
name: e2e-block                      # required
metadata:                            # dashboard-friendly test identity (optional)
  test_id: e2e-block
  team: block
  owner: block-qa
timeout: 20m                         # whole-scenario timeout (e.g. 30s, 20m)
env:                                 # variables, overridable with `run -env K=V`
  product_root: /tmp/seaweed_block
  image: sw-block:local

topology:                            # which machines actions can target
  nodes:
    m02: { host: 192.168.1.184, user: testdev, key: ~/.ssh/testdev_key }

phases:
  - name: build
    actions:
      - { action: exec, node: m02, params: { cmd: "make -C {{ product_root }}" } }

  - name: test
    actions:
      - action: dd_write
        target: vol0
        save_as: write_out           # capture output into a var
        retry: 2
        params: { size: 1G, bs: 1M }
      - { action: assert_contains, params: { in: "{{ write_out }}", want: "ok" } }

  - name: cleanup
    always: true                     # runs even if an earlier phase failed
    actions:
      - { action: assert_no_processes, node: m02, params: { match: "[w]eed" } }
```

## Top-level fields

| Field | Meaning |
|---|---|
| `name` | Scenario name (required). |
| `metadata` | Free-form map: `test_id`, `team`, `owner`, … → into `manifest.json` for the [dashboard](dashboard.md). |
| `timeout` | Whole-run timeout (`30s`, `5m`, `20m`). |
| `env` | Variable map; `{{ key }}` substitutes; `run -env K=V` overrides. |
| `cluster` | Attach-or-managed cluster declaration — see [Run Lifecycle](lifecycle.md#controlling-the-cluster). |
| `topology` | `nodes:` (name → host/user/key/alt_ips/is_local) and `agents:` (coordinator mode). |
| `targets` | Named block targets (ports, IQN/NQN, vol/wal size) for block scenarios. |
| `phases` | The ordered work (below). |
| `artifacts` | `on_failure:` paths + `dir:` to collect when a run fails. |

## Phase fields

| Field | Meaning |
|---|---|
| `name` | Phase name. |
| `actions` | List of actions (below). |
| `parallel` | `true` → run this phase's actions concurrently. |
| `always` | `true` → run even if a previous phase failed (use for `cleanup`). |
| `repeat` / `aggregate` / `trim_pct` | Run N times; aggregate `median`/`mean`; trim outliers (perf sampling). |
| `include` / `include_params` | Pull phases from another file with variable overrides. |

## Action fields

| Field | Meaning |
|---|---|
| `action` | Registered action name (`exec`, `dd_write`, `assert_contains`, …). `<binary> list` shows all. |
| `node` | Topology node to run on. |
| `target` / `replica` | Named block target / replica the action operates on. |
| `params` | Inline key/values for the action (`size`, `cmd`, `match`, …). |
| `save_as` | Capture the action's output into a variable for later `{{ }}` use. |
| `retry` / `timeout` | Retry count; per-action timeout. |
| `ignore_error` | `true` → a failure doesn't fail the phase. |

## Variables

- `env:` values and `-env K=V` overrides.
- `save_as:` outputs from earlier actions.
- injected by the runner: `run_id`, `bundle_dir`, `artifacts_dir`.
- referenced anywhere with `{{ name }}`.

Validate before you run — no lab needed:

```bash
<binary> validate scenarios/public/e2e-block.yaml
```
