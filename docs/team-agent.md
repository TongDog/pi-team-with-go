# 前后端 Agent 协作与经验复用

`pigo team` 使用现有 Go Agent Runtime，在独立对话上下文和 Git worktree 中运行前端、后端任务。双方共享明确的需求、验收条件和接口契约；控制器汇总结果、集成符合文件归属的修改、运行验收，并把失败证据交给对应角色修复。验收通过后，经验整理角色产生待确认条目，后续任务只召回已批准的经验。

这是本地 CLI 工作流。独立上下文与工作目录提供逻辑隔离，**不是操作系统安全沙箱**；模型使用的 shell 和验收命令具有启动用户的权限。请对你授权执行的项目使用此功能。原始 checkout 不会自动应用生成的修改；交付物保留在集成 worktree 和 patch 中供检查。

## 构建与准备

在本仓库根目录运行，Go 版本需满足 `go.mod`：

```sh
go build -o ./bin/pigo ./cmd/pigo
go build -o ./bin/pigo-experience ./cmd/pigo-experience
go test ./internal/team/... ./internal/experience ./internal/cli/teamcmd
```

待开发项目必须是有提交的 Git 仓库，启动时没有已跟踪或未跟踪的工作区修改。默认运行目录是 `<project>/.pigo/team/runs/<UTC时间>-<随机后缀>`；项目中的 `.pigo/` 必须被 Git 忽略。可以在项目 `.gitignore` 添加 `.pigo/` 后提交，也可以选择仓库外的 `--run-dir`。运行目录必须是尚不存在的新目录。依赖安装、测试工具和外部服务需要事先准备好；worktree 不会复制忽略的 `node_modules`、本地环境文件或未提交材料。

凭证通过现有 provider 环境变量解析，或者用 `--api-key-env` 指定环境变量名。不要把密钥写入 spec。显式选择的密钥不会写入状态、事件日志或命令参数；模型可能通过普通工具接触宿主环境，因此请勿把工作目录当作凭证隔离边界。

## 一次开发任务

`examples/team/spec.json` 定义了一项固定的用户列表功能。`project` 是经验库的稳定项目标识；`--project` 是本机 Git 根目录，两者用途不同。契约的 `response` 可以是明确的响应示例或 schema；真正的契约检查必须由 `checks` 中的可执行验收程序完成，控制器不会自动把任意 JSON 推导为全面的测试。

```sh
./bin/pigo team run \
  --spec ./examples/team/spec.json \
  --project /absolute/path/to/project \
  --model YOUR_MODEL \
  --api-key-env YOUR_PROVIDER_KEY_ENV \
  > result.json
```

也支持 `--base-url`、`--protocol`、`--provider`、`--thinking`。它们沿用现有 provider 解析规则，恢复任务时需要重新提供；模型凭证不保存进运行状态。运行阶段日志实时写入 stderr，因此重定向 stdout 不会遮住进度。

`frontend.paths` 与 `backend.paths` 不允许重叠，双方修改必须位于各自归属目录。共享配置、验收脚本等应在开始前明确分配或预先准备。集成会拒绝越界文件修改，不会静默覆盖另一方代码；但这是一项交付检查，不会阻止拥有 shell 权限的进程在执行中访问其他路径。

开发角色会明确加载项目根 `AGENTS.md` 和归属目录及其祖先的模块规则，并在日志中记录加载阶段；不会自动注入对方模块规则。定位到更深的子目录后，需要通过读取工具继续确认该目录规则。模块规则加载超过 128 KiB 会报错，不会静默截断。历史经验以证据建议提供，规则或契约存在冲突时要求角色返回阻塞原因。

`checks[].command` 是 argv 数组，不会隐式传给 shell。`dir` 相对集成 worktree，`timeout_seconds` 控制超时，`owner` 取 `frontend`、`backend` 或 `both`，决定失败证据交给谁。需要管道时应显式提供经过审阅的脚本。验收在集成 worktree 执行，模型仅仅报告 ready 不会使流程通过。

## 状态、恢复与契约修改

从 setup 日志或结果文件定位本次运行目录：

```sh
./bin/pigo team status --run-dir /absolute/path/to/run
./bin/pigo team resume --run-dir /absolute/path/to/run \
  --model YOUR_MODEL --api-key-env YOUR_PROVIDER_KEY_ENV
```

`status` 只读取状态，不调用模型。Ctrl+C 会取消当前执行，已写入的状态、工作目录和证据保留；`resume` 从已有状态继续，复用同一契约下已 ready 的角色实现，受中断角色可能重新运行当前步骤。验收仍然会针对新的集成结果执行。自动修复由 `max_repair_rounds` 限制，预算耗尽会阻塞并保留证据；再次 resume 不会无限自动补充修复预算，需要先检查或人工修复对应角色 worktree。

Agent 需要修改接口时，只能提出下一版契约并暂停。检查 `contract-proposal-frontend.json` 或 `contract-proposal-backend.json`，确认完整内容后显式接受：

```sh
./bin/pigo team contract --run-dir /absolute/path/to/run \
  --file /absolute/path/to/accepted-contract-v2.json
./bin/pigo team resume --run-dir /absolute/path/to/run \
  --model YOUR_MODEL --api-key-env YOUR_PROVIDER_KEY_ENV
```

传入的是完整 `Contract` JSON 对象，而不是完整 spec；版本必须恰好是当前版本加 1。接受操作会记录事件、让双方重新验证实现，但不自动调用模型。若验收预期也发生实质变化，应同步审查测试要求；接受契约不会自动改写你提供的测试程序。

## 命令行阶段日志与文件

stderr 和 `events.jsonl` 会显示对齐、经验召回、双方开发、工具运行、集成、验收、修复、经验提炼和交付的进展。事件包含 UTC 时间、run ID、阶段、角色、契约版本、尝试轮次、状态和消息。创建状态文件前的 setup 信息只写 stderr；创建完成后的阶段事件同时落盘。共享事件日志不包含完整模型会话和工具参数。

stdout 返回一个 JSON 状态对象。退出码 `0` 表示成功或帮助，`2` 表示参数/输入格式错误，`1` 表示执行失败或需要处理的阻塞。经验子命令沿用其自身 JSON 返回值。运行目录包含：

| 路径 | 用途 |
|---|---|
| `state.json` / `spec.json` | 当前状态与输入规格 |
| `contract-vN.json` | 已接受的契约版本 |
| `events.jsonl` | 可逐行读取的阶段事件 |
| `private/<role>-NNN.json` | 每个角色的独立会话记录，不自动发送给另一角色 |
| `workspaces/frontend` / `workspaces/backend` | 角色工作目录 |
| `workspaces/integration-N` | 第 N 次集成结果 |
| `patches/integration-N.patch` | 可供人工审阅和应用的修改 |
| `checks/NNN-NN.log` | 验收命令输出，失败反馈保留其路径 |
| `workspace-owner.json` | 运行目录及基线提交的归属记录 |

检查结果中的 `integration_dir` 与 `patch`；通过 `git diff --cached` 检查集成 worktree 中的修改。确认后可以在目标项目手动执行 `git apply --check /path/to/integration-N.patch`，再决定是否应用。此命令不会自动提交、推送或部署。

私有会话和验收输出可能包含项目源代码或命令输出。复用经验时只需共享经确认的条目与可访问的证据，不应把整个运行目录作为跨 Agent 的公共上下文。

## 经验确认、检索与停用

经验默认保存到 `<project>/.pigo/team/experience`。提炼阶段允许返回零条候选，不会为凑数量把一次成功执行概括成未经证实的经验。条目包括问题、解决方式、适用范围、模块、来源、契约版本与接口指纹；有证据时附根因和后续验证风险。批准操作是明确的用户动作，提炼 Agent 没有批准工具。

```sh
./bin/pigo team experience --root /project/.pigo/team/experience list --project team-users-demo
./bin/pigo team experience --root /project/.pigo/team/experience get ENTRY_ID
./bin/pigo team experience --root /project/.pigo/team/experience approve ENTRY_ID
./bin/pigo team experience --root /project/.pigo/team/experience search \
  --project team-users-demo --query 'users response' --modules frontend,backend
./bin/pigo team experience --root /project/.pigo/team/experience revise ENTRY_ID revised-entry.json
./bin/pigo team experience --root /project/.pigo/team/experience history ENTRY_ID
./bin/pigo team experience --root /project/.pigo/team/experience retire ENTRY_ID
```

只召回已批准且未停用的条目，项目范围必须明确。修订后的经验需要重新批准。历史经验是建议，不能覆盖当前契约；协作控制器按接口指纹检查兼容性，避免把不同任务的“v1”视为同一接口。带契约版本但缺少指纹的历史条目要求复核，未标注版本的已审核通用建议可参与召回。兼容性过滤先于结果数量限制。指纹只覆盖接口定义，不能等同于自动判断依赖和代码版本兼容性。

第一版使用本地结构化文件和可解释的文本/模块匹配，不依赖数据库或向量服务。多人身份权限、远程同步和生产级多租户不在当前能力范围内。

## 其他 Coding Agent 的接入

CLI 和原生 pigo 插件支持以下接入方式：

1. 宿主工具调用同一 CLI 的 `experience propose/search/get`，读 stdout JSON、把 stderr 用于诊断。
2. 支持 pigo 外部插件协议的宿主启动下面的进程，通过 stdio 调用其工具：

```sh
/absolute/path/to/pigo team experience --root /project/.pigo/team/experience serve
```

协议为每行一个 JSON-RPC 2.0 消息，方法有 `initialize`、`tools/call`、`shutdown`。例如：

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"experience_search","arguments":{"project":"team-users-demo","query":"users"}}}
```

工具名是 `experience_propose`、`experience_search`、`experience_list`、`experience_get`。这不是 MCP 协议；其他宿主需要 CLI 工具适配或协议桥接。接口可复用不代表已经验证所有 Coding Agent 的自动生命周期接入。CLI/协议负责经验保存与查询；外部宿主负责何时采集证据、调用提炼模型，以及何时检索。

标准 MCP 宿主使用独立的 `mcp` 模式，具体配置与协议范围见 [MCP 接入说明](experience-mcp.md)。`serve` 保持原有 pigo 协议，不能直接作为 MCP 服务启动。

## 无密钥链路验证

下列测试启动本地模拟模型服务，经真实 HTTP/SSE、Runtime 工具执行、Git 工作目录和 Shell 验收完成开发、失败修复与经验草稿流程。`-v` 会打印完整阶段日志；不访问在线模型，不需要模型密钥，需要允许监听本机端口。

```sh
go test -v ./internal/cli/teamcmd -run '^TestRunCLIProviderSSEDevelopmentRepairExperience$' -count=1
go test -v ./internal/team -run '^TestUsersExampleJointHTTPAcceptanceAndRepair$' -count=1
go test -race ./internal/team/... ./internal/experience ./internal/cli/teamcmd
```

模拟模型固定返回工具调用，因此该测试证明运行链路和故障处理，不证明真实模型的编程质量、提速幅度或经验带来的收益。真实任务应另行记录模型、任务、验收结果、修复次数和调用成本。

第二个测试用脚本 Worker 修改随仓库提供的用户列表示例，运行真实 Go HTTP 服务与 JavaScript 请求函数，验证第一次接口不一致能被发现、第二次修复能通过。它验证示例验收本身，不访问在线模型，也不是浏览器自动化。

## 本地示例

`examples/team/project` 是故意未完成的起始项目，验收脚本在开发前应失败。需要 Go 和 Node.js 18+，无 npm 依赖。复制到独立目录并创建初始提交，避免直接把本仓库当作示例的开发对象：

```sh
cp -R examples/team/project /private/tmp/team-users-demo
git -C /private/tmp/team-users-demo init
git -C /private/tmp/team-users-demo add .
git -C /private/tmp/team-users-demo commit -m 'Initialize users demo'
./bin/pigo team run --spec examples/team/spec.json \
  --project /private/tmp/team-users-demo --model YOUR_MODEL \
  --api-key-env YOUR_PROVIDER_KEY_ENV
```

目标是 Go HTTP 服务返回用户列表，浏览器页面调用同源接口并安全渲染姓名。检查包含前端逻辑、Go 测试以及真实后端进程与前端请求函数的联合验收；后者使用 Node 的 fetch，不是浏览器自动化。验收脚本位于双方归属目录之外，不能通过交付修改测试来绕过要求。示例不代表任意技术栈或任意需求的成功率。
