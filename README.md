# Focus-flash

Focus-flash 的 Go 核心包运行一个 Agent 会话。调用方可在执行期间追加引导消息和后续消息。业务流程记录在本地维护文档 `DecisionTree.md` 中。

## 接入

- 实现 `agent.Model.Generate`，从 `ModelRequest` 读取会话和工具描述，用 `EmitText` 交付文本片段，用 `SubmitTools` 在一轮内交付一组完整的工具调用。两个回调应在 `Generate` 所在的 goroutine 中调用，并在它返回前结束。
- 通过 `tool.NewOrchestrator` 注册工具实现和可选的参数校验器。编排器检查工具名称和 JSON 对象格式。并发上限按批次配置，默认顺序执行。并行运行的工具和校验器应支持并发调用。工具实现应响应上下文取消。
- 用 `agent.New` 注入模型及可选的工具编排器。用 `Run` 执行任务、用 `Steer` 追加引导消息、用 `FollowUp` 追加后续消息。若配置了 `OnEnd`，`Run` 会等它处理完再返回。

## 验证

使用 Go 1.22 或以上版本。测试只有 Go 标准库依赖，无需额外安装。

```sh
go test ./...
go vet ./...
```

在支持 cgo 和 C 编译器的环境中跑竞态检测。

```sh
go test -race ./...
```
