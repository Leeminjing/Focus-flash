# Patrol 架构

Patrol 为工作区提供一个对话入口。Windows 宿主通过 `app.NewPatrol` 装配对话模型、工作会话模型、Recorder 模型和工作会话可用的工具。Patrol 通过 session-core 的公开接口创建和运行 Agent。 [组合根](../app/app.go) · [Session core](./session-core.md)

```mermaid
flowchart LR
    subgraph Host["Windows 宿主"]
        UI["工作区对话入口"]
        App["app.NewPatrol<br/>装配模型和工具"]
    end

    subgraph PatrolLayer["Patrol · 工作区调度"]
        P["Patrol<br/>派生和派发"]
        S["Set<br/>工作会话清单"]
        R["Round<br/>一条输入触发的工作 Run"]
        O["Observation<br/>会话状态与用户输入历史"]
        Rec["Recorder<br/>原始归档与工作记忆"]
    end

    subgraph Core["session-core · 单会话执行"]
        C["人与 Patrol 的 Agent 会话<br/>每轮新建"]
        W["工作 Agent 会话<br/>跨轮保留"]
    end

    UI -->|"提交输入 · 接收进度与结果"| P
    App -->|"注入模型与工具"| P
    P -->|"启动决策会话"| C
    C -->|"create_context / send_message"| P
    P -->|"登记和查找工作会话"| S
    S -->|"按会话 ID 持有"| W
    P -->|"归组实际派发"| R
    R -->|"按派发顺序执行"| W
    R -->|"工作 Run 的新增消息"| Rec
    W -.->|"按需补读归档"| Rec
    S -->|"所有工作会话 ID"| O
    Rec -->|"最新归档与工作记忆"| O
    O -->|"供下一轮判断"| P

    classDef focus fill:#d9ebff,stroke:#2563a8,stroke-width:2px,color:#11243b
    class P focus
```

## 一轮如何推进

1. Patrol 用上一轮的 Observation 和本次用户消息开启一个新的决策会话。需要派生时，`create_context` 创建工作会话并写入交接消息。`send_message` 派发任务后，新会话的首个模型请求同时包含交接消息和任务。
2. Round 汇集这次派发实际触发的 Run。人与 Patrol 的决策会话在本轮派发完成后结束使用。工作会话继续执行，宿主通过 `InstantMessage` 接收进度。
3. 工作 Run 全部结束后，Recorder 追加原始归档，并结合上一版记忆更新各会话的工作记忆。Patrol 列出 `Set` 里的所有工作会话，连同最新归档、记忆和用户原文生成新的 Observation。下一条输入等待它发布。

工作会话、归档和记忆存放于当前 Patrol 实例的进程内存。 [Patrol](../patrol/patrol.go) · [工具](../patrol/tools.go) · [Recorder](../patrol/recorder.go)
