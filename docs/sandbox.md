# Windows 沙箱

在项目根目录的 `config.yaml` 中用顶层字段 `sandbox_mode` 选择模式。`app.New` 以 `projectDir` 作为工作区，并将模式交给 Windows 内置 Tool 编排器。修改后，重启宿主或重新调用 `app.New` 即可生效。

```yaml
model:
  name: example-model
sandbox_mode: workspace-write
```

| 模式 | Shell 准入 | 执行期文件权限 |
| --- | --- | --- |
| `danger-full-access`（缺省） | 使用宿主可访问的路径 | 使用宿主原始权限 |
| `read-only` | 检查工作目录、命令中的显式路径和目录切换 | 普通文件可按 Windows 权限读取，写入、删除和重命名受限。`edit` 在准入阶段拒绝 |
| `workspace-write` | 使用与 `read-only` 相同的路径检查 | 可写当前工作区和本次任务的私有临时目录。Shell 通过 `TEMP`、`TMP` 获取临时目录 |

## 命令准入

受限模式使用 PowerShell 7 解析命令文本，检查显式路径及已有路径解析后的实际位置。路径检查会拦截显式的工作区外文件路径、`..` 路径段、`file://`、UNC 和设备路径。系统目录下的可执行文件可作为命令目标。目录切换需要明确的工作区内目标，子表达式里的已知目录切换会被拦截。

检测到 `PATH` 修改时记录告警，其余准入检查照常执行。动态表达式在命令运行时求值，实际文件写入仍受 Windows 执行期权限约束。

## 执行权限

受限模式使用低完整性和受限令牌。Windows 检查文件写权限时，同时检查宿主身份和令牌中的限制 SID，以及目标的完整性策略。`read-only` 的限制 SID 没有普通文件写授权。`workspace-write` 为当前工作区和本次任务的私有临时目录使用不同的能力 SID，并设置对应的 DACL 和完整性标签。Shell 子进程沿用受限安全上下文，宿主只向启动的进程传递必要的标准 I/O 句柄。`edit` 使用同一模式的受限令牌执行文件操作。

执行期写边界适用于具有正常安全描述符的本地 NTFS 普通文件。读取由 Windows 对象权限决定，网络访问由系统网络策略管理。

## 授权与恢复

`workspace-write` 的首次 `shell` 或 `edit` 调用通过准入后，会保存工作区权限的恢复记录，再准备工作区与临时目录的授权。准备失败时，调用返回错误。任务收尾时，编排器确认进程树停止，再恢复权限并清理私有临时目录。宿主异常退出后，下次准备相同或重叠的工作区时会尝试从恢复记录撤回授权。

## 验证状态

在 Windows 11 `10.0.26200.9974` 上，实测 PowerShell 7.6.9 在 `read-only` 和 `workspace-write` 下因 `BCrypt.dll` 初始化失败（`0x8007045A`）提前退出。该环境已单独验证原生 Windows 文件权限、受限 `edit` 和 `danger-full-access` 下的 PowerShell。

工具调用方式、会话状态和请求结构见 [tool](./tool.md)。
