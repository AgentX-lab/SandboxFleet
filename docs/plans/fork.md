# Sandbox Fork 计划

## 1. 目标

支持从正在运行的父 Sandbox 分出 N 个并行子 Sandbox（可跨节点）：

父短暂冻结 → checkpoint → 上传 S3 → 父自动恢复 → N 个子从同一快照启动。

## 2. 相对现状：新增什么

当前已有：Sandbox / SandboxPool、冷启动（image+command）、Slot 调度、Start/Stop/Exec/Files。

| 新增 | 作用 |
|---|---|
| `SandboxSnapshot` CR | 不可变 State Root，指向 S3 对象 |
| `Sandbox.spec.fromSnapshot` | 从快照启动（与 `container` 互斥） |
| `POST .../sandboxes/{name}/fork` | 一次完成：打快照 + 创建 N 个子 |
| `Pool.spec.snapshotStorage.s3` | S3 配置（凭证走 Secret） |
| Runtime：`Checkpoint` / `Restore` | 直连 runsc（不走 CRI） |
| Worker：`POST /snapshots`、`POST /sandboxes:restore` | 本机 C/R + 上下传 S3 |
| `SnapshotStore` | S3 Put/Get/Delete |
| Fork 编排逻辑 | 容量预检、Commit、建 child、失败回滚 |

不改：现有冷启动路径、Exec/Files、Slot 模型（仅多一种启动源）。

## 3. 核心概念

| 概念 | 含义 |
|---|---|
| Sandbox | 运行实例（父或子） |
| SandboxSnapshot | 不可变状态根，内容在 S3 |
| Fork | API：1 次 Commit + N 个子 |
| Frozen Commit | 冻父 → 打快照 → 上传 → 父自动 Resume |
| fromSnapshot | 子 Sandbox 的启动方式 |

约束：

- Fork 的源是 Snapshot，不是对 live 进程直接 clone
- 同一 Snapshot 可被多次引用
- 子是普通 Sandbox，调度不绑定父所在 Worker
- `container` 与 `fromSnapshot` 二选一

## 4. 对象关系

```text
Parent (Running)
    │
    │ Fork(N)
    │  Freeze → Checkpoint → Upload S3 → Resume Parent
    ▼
SandboxSnapshot  ──objectKey──►  S3
    │
    ├── Child₁ (任意空闲 Worker/Slot)
    ├── Child₂
    └── Childₙ
```

## 5. API

### 5.1 SandboxSnapshot

```yaml
spec:
  sourceSandbox: <parent>    # 不可变
  poolRef: <pool>            # 不可变
status:
  phase: Pending | Ready | Failed
  objectKey: ...
  digest: ...
  sizeBytes: ...
  sourceWorker: ...          # 仅记录谁上传，不参与调度
```

### 5.2 Sandbox（扩展）

```yaml
spec:
  poolRef: ...
  slotProfile: ...
  # 二选一：
  container: { image, command, ... }   # 现有冷启动
  fromSnapshot: <snapshot-name>        # 新增：快照启动
```

### 5.3 Fork（新增 subresource）

```http
POST .../namespaces/{ns}/sandboxes/{parent}/fork
```

```yaml
# 请求
count: N
slotProfile: <可选，默认继承父>
failurePolicy: DeleteChildren | Retain   # 默认 DeleteChildren

# 响应
snapshotName: ...
children:
  - name: ...
```

### 5.4 Pool（扩展）

```yaml
spec:
  snapshotStorage:
    s3:
      endpoint: ...              # 可空
      bucket: ...
      region: ...
      credentialsSecretRef:
        name: ...
```

e2e 使用 MinIO（S3 API）。

## 6. 分层结构

```text
Fork API / Snapshot CR / Sandbox CR
              │
         Controller
         （编排 Commit、建子、GC）
              │
         Scheduler
         （按 slotProfile 选任意空闲 Slot）
              │
          Worker HTTP
              │
     ┌────────┴────────┐
 Runtime (runsc)    SnapshotStore (S3)
 Checkpoint/Restore   Put / Get / Delete
```

Worker 两个关键动作：

- **Commit**：Freeze → `runsc checkpoint` → S3 Put → Resume
- **Restore**：S3 Get → `runsc restore` → 占用指定 Slot

## 7. 流程

### 7.1 Commit

```text
父 Running
  → Freeze
  → Checkpoint（本地）
  → Upload S3
  → Resume 父（回到 Running）
  → Snapshot Ready
```

失败：尽力 Resume 父；Snapshot 不得进入 Ready。

### 7.2 Fork

```text
1. 校验父 Running
2. 预检空闲 Slot ≥ N（不足则直接失败，不打快照）
3. Commit → 得到 Ready Snapshot
4. 创建 N 个 Sandbox（fromSnapshot）
5. 各子：Assign → Restore → Running
6. 返回 snapshotName + children
```

同一父的并发 Fork：串行。

### 7.3 删除

```text
删 Child     → 停运行时 + 释放 Slot；不删 Snapshot
删 Snapshot  → 无 fromSnapshot 引用时才删 S3
删 Parent    → 不级联删 Snapshot / Children
```

## 8. 失败策略

| 情况 | 处理 |
|---|---|
| Commit 失败 | 父已 Resume；无 Ready Snapshot；Fork 失败 |
| 容量不足 | Commit 之前失败 |
| 部分 Child 失败 | 默认删掉已创建 Child；或 Retain 并返回部分结果 |
| 某 Child Restore 失败 | 该 Child → Failed；不影响父与其它 Child |

## 9. 首期不做

- Live（不停机）checkpoint
- runc 的 C/R
- 增量快照
- 父删时级联清理

## 10. 实施顺序

1. S3 `SnapshotStore` + Pool 配置  
2. Runtime：runsc Checkpoint / Restore  
3. Worker：Commit / Restore HTTP  
4. CRD：SandboxSnapshot + `fromSnapshot`  
5. Controller：Snapshot 与快照启动  
6. Fork subresource  
7. e2e：MinIO + ≥2 Worker + 跨节点断言  

## 11. e2e 验收要点

- 父写文件后 Fork(2)
- 父仍 Running
- 两子 Running，且尽量落在不同 Worker
- 两子都能读到 Commit 前文件，各自修改互不影响
- 删子与 Snapshot 后 S3 对象清除
