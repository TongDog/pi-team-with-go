# 协作功能验证记录

本次交付在现有 pigo Runtime 上新增本地前后端协作与经验复用。保留上游运行时和基础工具的归属，不把它们计作本次新增实现。

## 需求与证据

| 需求 | 实现 | 验证证据 |
| --- | --- | --- |
| 需求、契约、归属和验收输入明确 | `team/spec.go` | `spec_test.go` 覆盖缺失验收、归属重叠、路径穿越、非法契约、未知字段、尾部数据和超限输入 |
| 前后端并行、上下文和文件目录独立 | `team/worker.go`、`team/workspace` | `worker_test.go` 使用真实 Runtime 和工具检查独立提示词/会话；`coordinator_test.go` 用并发屏障证明同时执行 |
| 项目和模块规则明确加载 | `team/rules.go` | `rules_test.go` 检查祖先顺序、目录作用范围、文件归属、去重、错误和大小限制；Runtime 测试确认对方模块规则未注入 |
| 共享版本化契约推进开发 | `team/state.go`、`team/coordinator.go` | 契约变更须显式接受，双方重新验证；旧版本结果被拒绝；并发控制器锁测试通过 |
| 真实验收控制交付、有限轮次修复 | `team/checks.go`、协调器 | CLI HTTP/SSE 端到端测试先失败后只修复后端；预算耗尽和人工修复后恢复测试通过 |
| 修改不直接覆盖源工作区 | 独立 Git worktree 与作用范围检查 | workspace 测试覆盖越界修改、链接、篡改元数据、脏源目录；两个端到端测试确认源目录保持不变 |
| 取消、超时与恢复 | 进程组取消、持久化状态、重试计数 | Runtime 活跃 Shell 取消、命令超时、取消后恢复、验收预算和过期结果测试通过 |
| 各阶段有命令行日志 | `team/log.go`、CLI | stderr 与 JSONL 阶段断言、并发日志完整性、日志写入失败不能宣告成功；记录规则加载、工具结束和失败事件 |
| 从执行证据提炼经验 | 无工具的 experience Worker | 断言收到实际补丁、契约内容、失败与成功验收记录；拒绝伪造来源；允许零条候选 |
| 经验审核、去重、修订与停用 | `experience/store.go` | 生命周期、并发写入、修订历史和旧版本写入拒绝测试通过；候选不自动审核 |
| 初次与定位后的经验召回 | 协调器和 `experience_search` | 审核状态、项目、模块、接口指纹过滤；相同 v1 不代表同一接口；12 条不兼容记录不能挤掉有效经验 |
| 可复用经验插件 | 独立 CLI、pigo JSON-RPC、MCP stdio | MCP 真实子进程握手、工具发现、查询、草稿、协议错误、通知和 EOF 退出测试通过；审核工具不暴露给模型 |
| 可运行的前后端示例 | `examples/team` | `TestUsersExampleJointHTTPAcceptanceAndRepair` 运行真实 Go HTTP 服务和 JavaScript fetch，首轮接口不一致失败、次轮修复通过 |

## 执行结果

以下检查已在 macOS arm64、Go 1.27rc1 环境通过。HTTP/SSE 模拟服务与示例后端需要本机端口权限；首次沙箱内执行因禁止监听失败，允许本机测试端口后重新执行成功，未跳过这些测试。

```sh
go test ./...
go test -race ./internal/team/... ./internal/experience ./internal/cli/teamcmd ./cmd/pigo-experience
go test -race ./internal/team -run '^TestUsersExampleJointHTTPAcceptanceAndRepair$' -count=1 -v
go vet ./internal/team/... ./internal/experience ./internal/cli/teamcmd ./cmd/pigo ./cmd/pigo-experience
go build ./cmd/pigo ./cmd/pigo-experience
GOOS=windows GOARCH=amd64 go build ./cmd/pigo ./cmd/pigo-experience
```

另对实际编译的命令检查了帮助输出、MCP 握手，并在保持 stdin 打开的情况下发送 SIGINT/SIGTERM：独立 MCP、`pigo team experience mcp` 和原生插件 `serve` 均及时退出。运行类命令捕获信号以保存状态；stdio 服务保留操作系统默认信号退出行为，避免等待输入时吞掉 Ctrl+C。

## 证据边界

- CLI 端到端测试使用本地模拟模型，经真实 HTTP/SSE 与工具循环执行；Go/JavaScript 示例使用脚本 Worker。两者证明工程链路，不证明真实大模型的任务成功率或效率提升。
- MCP stdio 已验证协议和真实进程通信，尚未验证具体第三方 Coding Agent 的 UI 安装和自动调用策略。外部宿主负责组织证据并生成候选，插件负责提交、查询和状态维护。
- 经验提炼是模型生成的候选，必须人工审核；接口指纹不等于代码和依赖版本兼容性。当前检索是文本与模块匹配，没有向量检索质量实验。
- worktree 和独立上下文是逻辑隔离，不是 OS 安全沙箱。Windows 做了交叉编译，尚未进行 Windows 运行验收。
- 本次交付面向单机已有 Git 项目，没有扩展成团队账户、远程服务或生产级多租户平台。

使用方式见 `docs/team-agent.md`，其他 Agent 的 MCP 配置见 `docs/experience-mcp.md`。
