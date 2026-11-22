# Tool 编排器与内置工具

## 编排器

Windows 宿主通过 `app.New` 装配 `shell`、`write_stdin` 和 `edit`。Agent 在每轮 `ModelRequest.Tools` 中发布编排器登记的工具名称、描述和参数 JSON Schema。模型接入层通过 `SubmitTools` 提交本轮完整的调用批次，每个调用包含 ID、工具名和 JSON 参数对象。

编排器按名称查找工具，检查参数格式，再执行适用的参数校验和准入。内置工具在批次内顺序执行，结果按原调用顺序返回。Agent 将结果和错误写入会话，供后续模型轮次读取。宿主可通过 `tool.NewOrchestrator` 注册受信 Go 回调及可选校验器，并设置每批并发上限。启用并行时，回调和校验器应支持并发调用。`Registration.Run` 应响应上下文取消。

以下 JSON Schema 对应代码中向模型公布的 `Definition.Parameters`。沙箱模式和路径准入见 [sandbox](./sandbox.md)。

## `shell`

用 PowerShell 7 运行 `command`，可按当前模式读写文件。装配时从标准安装目录或宿主 `PATH` 选择工作区外的 `pwsh.exe` 并核对主版本。准入解析和执行共用这个路径。找不到 PowerShell 7 时，调用返回错误。 [PowerShell 7 安装文档](https://learn.microsoft.com/en-us/powershell/scripting/install/install-powershell-on-windows)

`command` 必须含有非空白内容。`workdir` 省略或为空时使用工作区。`yield_time_ms` 省略时等候 1000 毫秒，到期后可用返回的会话 ID 继续查看输出。

```json
{
  "type": "object",
  "properties": {
    "command": { "type": "string" },
    "workdir": { "type": "string" },
    "yield_time_ms": { "type": "integer", "minimum": 0, "maximum": 10000 }
  },
  "required": ["command"],
  "additionalProperties": false
}
```

## `write_stdin`

用 `session_id` 引用同一次 `Agent.Run` 中的 Shell 会话。`chars` 按控制台输入代码页发送，换行也通过该字段传入。省略 `chars` 或传入空字符串时可轮询。设置 `close_stdin: true` 会在发送输入后关闭输入管道，向进程发送 EOF。`yield_time_ms` 的默认值和范围与 `shell` 相同。

```json
{
  "type": "object",
  "properties": {
    "session_id": { "type": "string" },
    "chars": { "type": "string" },
    "close_stdin": { "type": "boolean" },
    "yield_time_ms": { "type": "integer", "minimum": 0, "maximum": 10000 }
  },
  "required": ["session_id"],
  "additionalProperties": false
}
```

### Shell 结果

两项工具都返回 JSON 文本，其中 `output` 是本次新交付的标准输出和标准错误。进程继续运行时包含 `session_id`，进程完成时包含 `exit_code`。某次输出超出缓冲上限时，响应带有 `truncated: true`。

```json
{ "output": "ready\r\n", "session_id": "a9b0ed9ff129ab1801694b445284685a" }
```

```json
{ "output": "done\r\n", "exit_code": 0 }
```

任务完成、取消或模型失败时，编排器回收尚存的 Shell 会话。

## `edit`

`path` 指向现有 UTF-8 文件。工具在 `old_text` 恰好匹配一处时替换该处，保留 BOM、原有的单一换行格式和未编辑内容。空 `new_text` 删除匹配的文本。成功时返回文本结果。无匹配、多处匹配或提交失败时返回错误。若替换失败或遇到并发替换，错误可能包含同目录 `.focus-edit-*.bak` 的恢复位置。

```json
{
  "type": "object",
  "properties": {
    "path": { "type": "string" },
    "old_text": { "type": "string", "minLength": 1 },
    "new_text": { "type": "string" }
  },
  "required": ["path", "old_text", "new_text"],
  "additionalProperties": false
}
```
