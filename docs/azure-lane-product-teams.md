# 产品团队使用 Azure 测试通道（seaweed-cloud）

本页面向需要在 Azure 上测试自己产品的团队。它说明 Azure 通道目前能测什么、谁批准、怎么准备、怎么跑、
怎么确认零残留，以及新产品要接入时需要做什么。命令与配置键均来自 `cmd/seaweed-cloud`、
`provision/smoke/` 和 `provision/terraform/` 的源码（main e3c18c9）。
完整的生命周期细节以 `provision/smoke/README.md` 为准。

## 1. 现状：已经验证了什么，还没有什么

**已验证（SeaweedFS 自身）**

- 单 VM 冒烟（16 × 1 MiB SET + 立即精确 GET）。
- 10 VM 集群功能测试：1 台 meta D4s_v5，6 台 volume D2s_v5，3 台 client D2s_v5，跑 KV + S3 并发批量，
  精确读回，结束后 destroy 并零残留。
- 10 VM 读路径混沌（volume 重启、filer 重启、volume 冻结），以及副本 001 的混沌集 A/B。
- 每次运行结束都列出整个资源组（RG）确认零残留；10 VM 运行还会自动扫描订阅。单 VM 运行没有内置的订阅扫描，
  如政策要求，由操作者另做只读的订阅审计。

**还不具备（请先读）**

- **通道只跑 SeaweedFS 的测试包。** 控制器在资源创建后固定调用 SeaweedFS 云测试包，必需的产物固定为
  `weed`、`weed-volume`、`native.tar` 三个文件。区域固定为 `westus2`。输入的 `vm_size` 必须是
  `Standard_D2s_v5`，用于单 VM 以及 10 VM 布局中的 volume 和 client；10 VM 布局的 meta 固定为 `Standard_D4s_v5`。
  拓扑固定为 1 台或上述 10 台（固定的节点名和角色）。
- **没有“提交自己的测试脚本”的通用入口。** 场景 YAML 引擎（`docs/scenario-spec.md`、`docs/wiki/guides/*`）
  面向实验室主机（M01/M02），不在 Azure 上运行。
- 所以：
  - **基于 SeaweedFS 构建的产品**（替换上述三个产物即可）可以直接用现有测试包测，见第 4 节。
  - **在现有布局上加新的检查或进程**：需要先在测试包中加一个阶段（评审过的 PR），见第 6 节。
  - **需要不同的拓扑**（节点数、规格、角色）：不只是加阶段，还要修改 Terraform 布局和集群校验，是更大的改动，见第 6 节。

## 2. 角色与审批

| 事项 | 规则 |
|---|---|
| 启动权限 | 每次 `run` 都需要测试负责人（manager）发放的运行令牌（launch token）：一次一个运行，有明确时间窗 |
| 资源组 | `rg-codex02-azure-test-20261029` 专用于本通道。开始时必须为空（否则 `RG_NOT_EMPTY`），运行期间不允许外部写入或改标签 |
| 预算 | 默认项目 `default`：单次 ≤ $3，累计 ≤ $10。团队应申请自己的 `project` 预算（第 3 节），超出单次上限需带批准消息的 `budget_override` |
| 零残留 | 每次运行结束时 RG 内（10 VM 运行还包括订阅扫描）不得留有本次运行的资源；留有残留即运行未完成，必须清理并报告 |
| 凭据 | Azure 凭据只在执行运行的 Linux 控制器上。产品团队不需要、也不应持有订阅凭据 |

**推荐分工**：产品团队准备产物与期望结果，向测试负责人申请令牌与项目预算；由有 Azure 访问权限的操作者
（runner 席位）执行运行并交付结果包。团队若自行操作，也必须遵守本页全部规则。

## 3. 项目预算

预算写在控制器配置（`--config` 的私有 JSON）里，不写在 tfvars 里：

```json
"project": "block",
"budgets": {"block": {"per_run_cap_usd": 3, "cumulative_cap_usd": 20}},
"prior_estimated_usd": 0.0
```

- 不写 `project` 即为内置的 `default`（单次 $3，累计 $10），不需要 `budgets`；`budgets` 里不能重新定义 `default`。
- 其他项目名须匹配 `[a-z0-9][a-z0-9-]{0,49}`，且必须是 `budgets` 中的键，否则拒绝。
- `prior_estimated_usd` 是本项目此前的估算花费，由调用方如实填写。检查规则为
  `prior_estimated_usd + estimated_usd <= cumulative_cap_usd`。
- 单次检查：`run_declaration.budget_usd <= per_run_cap_usd`，`0 < abort_estimate_usd <= budget_usd`，
  `0 < estimated_usd <= abort_estimate_usd`。
- 需要超过单次上限时，加 `budget_override`，带批准消息 ID：
  `{"message_id": "...", "amount_usd": 10, "approver": "...", "message": "..."}`。它只提高这一次的单次上限，
  不提高累计上限。
- 预算门只检查数字，不判断谁有权批准；授权仍来自运行令牌。项目上限请先与测试负责人确认。

## 4. 用现有测试包测你的构建（不改代码）

### 4.1 控制器准备（一次性）

- Linux 控制器（`run`/`destroy`/`verify`/`cleanup` 只在 Linux 运行；`plan` 可在任何系统运行）。
- Terraform **正好 1.17.1**（azurerm 5.12.0 由已提交的 lock 文件固定），Azure CLI 已登录，Go 1.25+，OpenSSH。
- 构建：`go build -o /private/bin/seaweed-cloud ./cmd/seaweed-cloud`（或 `make build-seaweed-cloud`）。
- 一把客户端 SSH 密钥（ed25519），私钥放在绝对路径，公钥写进 tfvars。
- 控制器的公网 IPv4（/32），写进 `operator_cidr`。
- 若控制器是通过 WSL 调用 Windows 版 `az`：WSL 的 Windows 程序互操作可能被同一内核上的其他发行版重置，
  届时 `az` 退出码 126。每次运行前先确认 `/mnt/c/Windows/System32/cmd.exe /c ver` 能执行。

### 4.2 私有文件（不要提交、不要放共享盘）

**tfvars**（`provision/terraform/azure-validation` 的变量）：

```json
{
  "subscription_id": "<订阅 ID>",
  "resource_group_name": "rg-codex02-azure-test-20261029",
  "location": "westus2",
  "ssh_public_key": "ssh-ed25519 AAAA... operator",
  "operator_cidr": "<控制器公网 IP>/32",
  "image_version": "<固定的 Ubuntu 24.04 镜像版本>",
  "artifact_manifest_sha256": "<64 位十六进制>",
  "vm_size": "Standard_D2s_v5",
  "cluster": false,
  "process_cleanup_reserve_ms": 15000,
  "run_declaration": {"work_deadline": "2026-11-20T18:00:00Z", "hard_deadline": "2026-11-20T18:15:00Z",
                      "cloud_cleanup_reserve_seconds": 900, "budget_usd": 3, "abort_estimate_usd": 2.5}
}
```

- `cluster: true` 即 10 VM 布局；`false` 为单 VM。
- 截止时间：硬截止必须按整分钟对齐，距开始不超过 75 分钟（60 分钟工作 + 15 分钟清理），
  `cloud_cleanup_reserve_seconds` 在 600–1800 之间，且硬截止 ≥ 工作截止 + 清理预留。
- `host_keys`、`node_host_keys`、`run_id` 是保留键，填写即被拒绝；`TF_VAR_*`、`TF_CLI_ARGS*` 环境变量不生效。

**控制器配置**（未知键会被拒绝；所有路径必须是绝对路径）：

```json
{
  "source": "/private/sw-test-runner",
  "module_sha256": "<pins 输出>",
  "variables": "/private/run.tfvars.json",
  "variables_sha256": "<pins 输出>",
  "output": "/home/<user>/.local/state/seaweed-cloud/runs/<run-id>",
  "estimated_usd": 0.5,
  "prior_estimated_usd": 0.0,
  "project": "block",
  "budgets": {"block": {"per_run_cap_usd": 3, "cumulative_cap_usd": 20}},
  "cloud": {
    "ssh_key": "/private/client-key",
    "files": {
      "weed": {"path": "/private/artifacts/weed", "sha256": "<你的构建的哈希>"},
      "weed-volume": {"path": "/private/artifacts/weed-volume", "sha256": "<哈希>"},
      "native.tar": {"path": "/private/artifacts/native.tar", "sha256": "<哈希>"}
    }
  }
}
```

- **run ID**：取 `output` 目录名作为 Terraform `run_id` 和资源标签。请用**小写字母开头、最多 32 个字符**
  （字母、数字、连字符）。CLI 文档写的是 50，但 Terraform 模块只接受 32。
- `output` 的上级目录必须已存在且可写（0700），否则 `OUTPUT_PARENT_REFUSED`；`output` 本身必须不存在，
  否则 `INPUT_REFUSED`。10 VM 运行请放在 `~/.local/state/seaweed-cloud/runs/` 下，`cleanup --run` 才能找到它。
- `native.tar` 顶层必须是 `seaweedkv_native/`，原样使用，不要重新打包。
- 可选 `cloud` 键（仅 10 VM），二选一，不能同时使用：
  - 第一阶段读路径混沌：`"chaos": true`（volume 重启、filer 重启、volume 冻结，约 +8 分钟、+$0.15）；
  - 第二阶段混沌集：`"chaos_set": "A"` 或 `"B"`，必须同时设 `"replication": "001"`。
  `chaos` 与 `chaos_set` 同时出现，或 `chaos_set` 未配副本 001，测试包都会拒绝。

### 4.3 运行步骤

```sh
# 1. 固定模块与变量的哈希（本地，不调用云）
seaweed-cloud pins --source /private/sw-test-runner --variables /private/run.tfvars.json --json
# 把输出的 module_sha256 / variables_sha256 填入控制器配置

# 2. 本地检查（不调用云；会显示 plan.budget）
seaweed-cloud plan --config /private/config.json --json

# 3. 拿到测试负责人的令牌后，在令牌时间窗内启动
seaweed-cloud run --config /private/config.json --json
```

`run` 一条命令完成全部流程：生成 VM 主机密钥、确认 RG 为空、terraform init/plan/apply、测试包各阶段、
terraform destroy、RG 列表（10 VM 还有按本次标签的清扫和订阅扫描）。测试失败也一定会 destroy，
但 destroy 成功不会把测试失败变成 PASS。

**结果判读**：只有 `COMPLETE`（以及 `plan` 的 `PLAN_ONLY`、清理的 `EMPTY`）退出码为 0。下表是 **`run` 的结果**。
测试是否通过只看原始 `run` 的结果，请单独保留它。

| `run` 的结果 | 含义 | 处理 |
|---|---|---|
| `COMPLETE` | 测试通过且零残留 | 交付结果包（第 5 节） |
| `PACK_FAILED` | 资源正常，测试本身失败 | 看 `cloud-step-*.json` 和日志，按产品缺陷处理 |
| `INPUT_REFUSED`、`DEADLINE_REFUSED`、`RG_NOT_EMPTY`、`OUTPUT_PARENT_REFUSED` | 未创建任何资源就拒绝 | 修正输入后重试 |
| `APPLY_FAILED_OR_UNKNOWN`、`CREATE_UNRESOLVED` | 创建失败或结果未知 | 不要新建运行；先执行恢复命令；`CREATE_UNRESOLVED` 永远不会变成 `COMPLETE`，必须报告 |
| `CLEANUP_INCOMPLETE`、`RESOURCES_REMAIN` | 有残留 | 立即执行恢复命令，并报告剩余资源 ID |

### 4.4 恢复（控制器或会话中断后，任何新运行之前）

**单 VM 运行**：用原始控制器配置恢复（保留该私有配置和运行目录）：

```sh
seaweed-cloud destroy --config /private/config.json --json
seaweed-cloud verify --config /private/config.json --json
```

**10 VM 运行**：除上面两条外，也可以按 run ID 恢复。只有 10 VM 运行会写 `recovery.json`，单 VM 运行不能用这条：

```sh
seaweed-cloud cleanup --run <run-id> --dry-run --json
seaweed-cloud cleanup --run <run-id> --json
```

恢复使用私有运行目录里保存的状态。确认零残留之前不要删除运行目录。

**注意**：恢复命令返回 `COMPLETE` 只表示清理成功，不表示测试通过。原来失败或未知的测试结果不会因为恢复成功而改变。

## 5. 交付结果包

- 结果包放在团队约定的证据共享位置（`<owner>-<主题>-<日期>/`），附 `SHA256SUMS` 和 `RESULT.md`。`RESULT.md` 写明：
  令牌、源码与 runner 版本、开始和结束时间、结果码、关键指标、残留检查、估算费用和项目累计。
- **运行目录里的原始文件一律留在控制器上，不直接分发。** 每台 VM 的主机私钥会出现在多处：
  `host.auto.tfvars.json`、Terraform 状态（`*.tfstate*`）、`.terraform`、保存的计划文件 `smoke.tfplan`，
  以及步骤日志中 `terraform show -json` 的输出。私钥在 `custom_data` 里是 base64 编码的，二进制计划文件
  也无法用文本搜索检查，所以搜索 `PRIVATE KEY` 之类的字样**不能**证明一个文件是安全的。
- 结果包只放**经过审阅和脱敏的副本**：
  - 需要的步骤日志（失败或重试的步骤，以及最后的 RG 列表；10 VM 运行还有订阅列表）逐个审阅，删去或替换含密钥的部分；
  - 附一份记录，写明每个原始文件省略或脱敏了什么；
  - 脱敏副本用自己的 sha256。不要让人以为脱敏副本和原始文件是同样的字节。
- 计划文件、状态文件、`host.auto.tfvars.json` 和 `terraform show` 的输出绝不进入结果包。
- `actual_cost_usd: null` 表示账单未结算，不是零费用。

## 6. 新工作负载接入（需要改代码）

1. **先在本地或实验室跑通**：用 `seaweed-cloud direct --inventory INV.json --config CONFIG.json --out NEW_DIR`
   在已有主机（onprem 或 docker 清单）上运行。它不创建、不销毁云资源，是上云前的预演。
2. **在测试包里加一个阶段**：在 `packs/seaweed/cloud*.go` 增加你的阶段和对应的 `cloud` 配置键，
   附单元测试和一个强制失败的对照。参考 Draft PR #130–#132（10 VM 上的 mount soak）。
3. 以 Draft PR 提交，经评审后由维护者合并。
4. 合并后按第 4 节申请令牌运行。

以上只适用于在**现有布局**（1 台，或固定的 10 台）上加新检查。需要**不同拓扑**（节点数、规格或角色）时，
Terraform 布局（`provision/terraform/azure-validation`）和集群校验（`packs/seaweed/cloud_cluster.go`，
它检查固定的 10 个节点名和角色）都要改，并且需要单独评审和新的云上验证。请先找测试负责人讨论。

## 7. 已知的坑

- **cloud-init 等待**：测试包对 `cloud-init status --wait` 只等 120 秒，而 D2s_v5 上实测约 267 秒，
  会在 prepare 阶段以 `PACK_FAILED` 失败。可配置的等待在 Draft PR #132 中，合并前请注意。
- **run ID 长度**：最多 32 个字符，以字母开头（见 4.2）。
- **WSL 互操作**：见 4.1。
- **运行目录含私钥**：只在控制器上保存，绝不放共享盘。
- **会话中断**：先执行恢复（4.4），再开新运行；计算、磁盘和公网 IP 在清理前一直计费。

## 8. 参考

- `provision/smoke/README.md`：完整的 Azure 生命周期、10 VM、清理与混沌说明。
- `provision/smoke/BUDGET-POLICY.md`：项目预算键。
- `provision/smoke/AZURE-ARGV-AUDIT.md`：控制器执行的确切 `az` 命令。
- `provision/terraform/azure/README.md`：Terraform 模块输入与清单导出。
- `README.md` 中 “How to run `seaweed-cloud direct`”：在已有主机上预演。
