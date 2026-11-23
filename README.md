# Focus-flash

Focus-flash 有两条主线。

- **session-core** 提供单会话 Agent 核心循环。调用方可在执行期间追加引导消息和后续消息。Windows 宿主装配 `shell`、`write_stdin` 和 `edit`。 [核心循环说明](./docs/session-core.md)
- **[Patrol（实验）](./docs/patrol-architecture.md)** 探索以工作区为单位的单入口对话。用户与 Patrol 的对话也由 session-core 执行，具体会话的选择、派生和调度由 Patrol 负责。

Patrol 单向依赖 session-core，通过现有公开契约组合单会话能力。Patrol 的实现不得改动 session-core 的源码、公开契约或核心循环行为。

已实现的业务流程记录在本地维护文档 `DecisionTree.md` 中。

## 项目配置

在项目根目录创建 `config.yaml`。顶层接收 `model` 和可选的 `sandbox_mode`。`model` 的字段由模型构造器的入参类型定义。以下示例的构造器接收 `name`。

```yaml
model:
  name: example-model
```

宿主应用在启动时调用 `focusflash/app` 的 `app.New(projectDir, newModel)`。从项目根目录启动时，`projectDir` 可传 `"."`。示例中的 `newModel` 签名为 `func(ModelConfig) (agent.Model, error)`，它使用以下配置来创建模型。

```go
type ModelConfig struct {
    Name string `yaml:"name"`
}
```

`app.New` 读取文件、以 `projectDir` 为工作区装配内置工具，再通过 `agent.New` 注入模型和 Tool 编排器。缺少文件或 `model`、YAML 格式错误、未声明的字段、无效的模式或模型构造失败都会中止初始化。编辑配置后，重启宿主或重新调用 `app.New` 即可应用配置。每次初始化会创建独立的 Agent 会话。

## Windows 沙箱与工具

- 配置三种沙箱模式，了解命令准入、执行权限和授权恢复。 [sandbox](./docs/sandbox.md)
- 了解工具编排器、Shell 会话和内置工具的 JSON Schema。 [tool](./docs/tool.md)

## 接入

- 实现 `agent.Model.Generate`，从 `ModelRequest` 读取会话和工具描述，用 `EmitText` 交付文本片段，用 `SubmitTools` 在一轮内交付一组完整的工具调用。两个回调应在 `Generate` 所在的 goroutine 中调用，并在它返回前结束。
- 用 `agent.New` 注入模型及可选的工具编排器。用 `Run` 执行任务、用 `Steer` 追加引导消息、用 `FollowUp` 追加后续消息。若配置了 `OnEnd`，`Run` 会等它处理完再返回。

### 工作区 Patrol

Windows 宿主用 `app.NewPatrol(projectDir, newModel)` 为工作区启动一个 Patrol。组合根从现有的 `model` 段分别构造对话、工作会话和 Recorder 的模型。

每条用户输入调用一次 `runtime.Run(ctx, input, onProgress)`。可选的 `onProgress func(patrol.InstantMessage)` 在运行期间接收进度。调用在本轮工作 Run 全部结束并发布 `Observation` 后返回 `patrol.Round`。同一工作区的工作 Run 按派发顺序执行，期间到达的下一条用户输入等待本轮观察发布。

工作会话、用户输入历史、归档和工作记忆存放在该 Patrol 实例的进程内存中，新建实例从空工作区会话开始。

## 验证

使用 Go 1.22 或以上版本。内置工具测试需要 Windows 10 或以上版本、本地 NTFS 和 PowerShell 7，可在干净检出中运行以下命令下载依赖并验证。

```sh
go mod download
go test ./...
go vet ./...
```

在支持 cgo 和 C 编译器的环境中跑竞态检测。

```sh
go test -race ./...
```
