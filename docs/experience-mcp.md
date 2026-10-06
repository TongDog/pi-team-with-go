# Experience 插件的 MCP 接入

本文依次说明构建与配置、工具行为、审核流程、协议范围和验证方法。

`pigo-experience --root DIR mcp` 将现有经验存储暴露为 MCP stdio 服务。其他支持 MCP stdio 的 Coding Agent 可以查询已审核经验、读取条目并提交新的经验草稿。宿主负责从开发记录中提炼候选内容；该服务不调用模型，不会主动读取宿主的对话，也不会自动把草稿变成已审核经验。

原有 `serve` 子命令继续使用 pigo 自己的插件 JSON-RPC 协议。MCP 客户端必须使用 `mcp`，两种模式不能互换。

## 构建与宿主配置

在仓库根目录使用满足 `go.mod` 要求的 Go 工具链构建：

```sh
go build -o ./bin/pigo-experience ./cmd/pigo-experience
```

在宿主中添加一个本地 stdio MCP 服务，填写以下值：

| 配置项 | 示例 |
| --- | --- |
| 名称 | `pigo-experience` |
| 可执行程序 | `/absolute/path/to/pi-team-with-go/bin/pigo-experience` |
| 参数 | `--root`, `/absolute/path/to/project/.pigo/team/experience`, `mcp` |
| 传输方式 | `stdio` |

若宿主采用 `mcpServers` JSON 配置格式，可使用如下配置。不同宿主的配置文件位置和外围字段不同，按其文档放置；路径必须替换为本机真实绝对路径。

```json
{
  "mcpServers": {
    "pigo-experience": {
      "command": "/absolute/path/to/pi-team-with-go/bin/pigo-experience",
      "args": [
        "--root",
        "/absolute/path/to/project/.pigo/team/experience",
        "mcp"
      ]
    }
  }
}
```

多个宿主指定同一个经验目录即可复用同一份本地条目。项目使用显式 `project` 字段区分；它是检索范围，不是多用户权限隔离。该进程以本地用户权限访问指定目录，不需要模型 API Key。不要把它当作面向不受信任客户端的远程服务。

## 工具与经验生命周期

| 工具 | 用途 | 可见状态 |
| --- | --- | --- |
| `experience_search` | 按项目、关键词和模块检索，参数为 `project`、可选 `query` / `modules` / `limit` | 仅 `approved` |
| `experience_propose` | 提交问题、方案、适用条件及证据来源，可附模块、契约信息、根因和验证风险 | 新条目为草稿；相同条目复用现有状态，不能审核 |
| `experience_list` | 列出指定项目的条目及状态 | 包括草稿和停用条目 |
| `experience_get` | 按 ID 读取条目、状态和来源 | 包括草稿和停用条目 |

`list` / `get` 用于检查记录，返回草稿不代表它可以作为已验证建议使用。检索结果中的来源与适用条件也需要结合当前代码检查；经验不会自动覆盖当前需求或接口契约。

提议工具的必填参数是 `project`、`problem`、`solution`、`applicability`、`sources`。`root_cause` 只在证据支持时填写；`risks` 可列出后续使用时需要验证的风险。`contract_version` 和 `contract_hash` 用于保留契约适用信息。通用 MCP 查询不替宿主决定当前契约是否匹配，宿主应检查这些字段；team 协调器另有契约适用性检查。

模型不能通过 MCP 调用 `approve`、`retire` 或 `revise`。成员阅读候选及原始依据后，在终端执行审核：

```sh
./bin/pigo-experience --root /absolute/path/to/project/.pigo/team/experience list --project my-project
./bin/pigo-experience --root /absolute/path/to/project/.pigo/team/experience get ENTRY_ID
./bin/pigo-experience --root /absolute/path/to/project/.pigo/team/experience approve ENTRY_ID
```

建议给宿主补充工作约定：开始方案设计时查询相关已审核经验；完成修复后，仅依据实际失败、修改和验证记录提交候选；证据不足时不推断根因。是否自动调用工具及调用时机由宿主控制，本服务没有安装宿主生命周期 hook。

## 协议范围

实现以官方 [MCP 2025-11-25 生命周期规范](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle) 为基准，只声明 `tools` 能力。支持 `initialize`、`notifications/initialized`、`ping`、`tools/list` 和 `tools/call`。客户端请求其他版本时，服务端返回自己支持的 `2025-11-25`；客户端应按规范决定能否继续连接。

按官方 [stdio 传输规范](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)，每条消息占一行，stdout 只输出 JSON-RPC 消息，运行日志写 stderr。关闭 stdin 后服务退出。没有额外的 `shutdown` RPC，也不提供 HTTP/SSE、resources、prompts、sampling、tasks 或工具列表变更通知。

工具定义使用 `inputSchema`，结果使用标准 `content: [{"type":"text","text":"..."}]`。文本中包含条目的 JSON，便于宿主读取。未知工具及格式错误返回 JSON-RPC 错误；参数校验和存储业务错误返回 `isError: true`，遵循官方 [工具错误处理约定](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)。审核相关工具既不出现在列表中，也不能直接调用。

单条输入上限为 4 MiB；不接受 batch。操作串行执行，适用于本地经验文件，不提供长任务进度或运行中请求取消。需要强制停止时，由宿主终止子进程。所有通知均不返回响应；未知通知被忽略。

## 验证与当前边界

离线测试启动真实子进程，通过 stdin/stdout 完成握手、初始化通知、ping、工具发现、检索、草稿提交和 EOF 退出。测试同时检查已审核状态过滤、审核工具拒绝、错误格式、版本协商、通知无响应、stdout 无日志污染，以及原有 pigo `serve` 行为。

```sh
go test -race ./internal/experience ./cmd/pigo-experience
```

也可以对编译后的命令手动发送以下消息，检查真实协议输出。初始化通知没有对应响应；示例检索在空库中返回空数组。

```sh
./bin/pigo-experience --root /tmp/pigo-experience-mcp-demo mcp <<'JSONL'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"manual-check","version":"1.0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"ping"}
{"jsonrpc":"2.0","id":3,"method":"tools/list"}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"experience_search","arguments":{"project":"my-project","query":"pagination"}}}
JSONL
```

上述验证证明本地协议与进程链路可运行；尚未实际验证特定第三方 Coding Agent 的 UI、安装发现或自动调用行为。接入具体宿主后，应另外确认握手成功、四个工具可见、已审核经验可检索以及草稿不会被自动采用。
