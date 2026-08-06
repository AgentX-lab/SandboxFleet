# Sandbox Fork 计划

## 这是什么

从**正在运行的父 Sandbox** 复制出 N 个**子 Sandbox**：

1. 父短暂停一下 → 打内存快照 → 上传到对象存储 → 父继续跑  
2. N 个子从**同一份快照**启动（可跨 Worker，但必须同 runtime）

子 Sandbox **和普通 Sandbox 一样占一个 Slot**，只是启动方式从「冷启动」变成「从快照恢复」。

**硬约束**：

- 真·内存 fork（禁止冷启 + 拷文件冒充）
- fork / restore 出来的子 **必须能出网**
- **对象存储格式与 substrate 一致**（见下文「快照怎么存」）
- **支持嵌套 fork**：从 `fromSnapshot` 恢复出来的子也可以再打快照、再 fork（gVisor / Kata 均支持）

---

## 名词表（先看这个）

| 名字 | 通俗解释 |
|------|----------|
| **Sandbox** | 一个沙箱实例（父或子），占 Worker 上一个 Slot |
| **Slot** | Worker 上的一个「坑位」，一个 Sandbox 占一个 Slot |
| **SandboxSnapshot** | 一份已上传的快照（K8s 资源，像一张「存档卡」） |
| **fork** | 用户操作：给父打存档 + 开 N 个子 |
| **fromSnapshot** | Sandbox 字段：「从这个存档启动」，不是冷启动 |
| **runtime** | 运行时类型：`runsc`（gVisor）或 `kata`，快照绑死这个值（Pool/CRI 里仍叫 `runtimeHandler`） |
| **storagePath** | 对象存储里的「目录路径」，一份快照对应一个路径下的多文件 |
| **manifest.json** | 快照自带的说明书：有哪些文件、什么 runtime、父是谁 |
| **snapshotFiles** | manifest 里列出的快照文件名（不含 `.zstd` 后缀） |
| **CreateSnapshot** | Worker：暂停父 → 本地打快照 → 按文件上传 → 恢复父 |
| **RestoreFromSnapshot** | Worker：拉 manifest → 按列表下载解压 → Slot 上 Load |
| **SaveSnapshot** | 适配器：只在 Worker 本机目录写出 checkpoint 文件 |
| **LoadSnapshot** | 适配器：从本机目录恢复成可 Exec 的运行实例 |

---

## 对象关系（一张图）

```text
父 Sandbox（Running，占 Slot #3）
    │
    │ fork(count=2)
    ▼
SandboxSnapshot「fork-abc」
  storagePath: snapshots/my-agent/abc/
    │
    ├── S3/MinIO（substrate 同款布局）
    │     manifest.json
    │     checkpoint.img.zstd
    │     config.json.zstd          # Kata 时可能有多个
    │     sandboxfleet-meta.json.zstd
    │
    ├── 子 Sandbox「child-1」（Slot #5，fromSnapshot）
    └── 子 Sandbox「child-2」（Slot #8，fromSnapshot）
```

---

## 快照怎么存（与 substrate 对齐）

**不是** 打成一个 `tar.gz`。

substrate（atelet）的做法：

1. ateom 在本机 **checkpoint 目录** 写出若干**普通文件**
2. 列出目录里有哪些文件 → 写入 `manifest.json` 的 `snapshotFiles`
3. **每个文件单独上传**为 `<文件名>.zstd`（zstd 压缩；内存稀疏文件用 sparse-extent 格式，洞不传）
4. **最后**上传 `manifest.json`

SandboxFleet **同样采用这套布局**（S3/MinIO/GCS 均可，接口抽象为 SnapshotStorage）：

```text
s3://<bucket>/<storagePath>/
├── manifest.json                 # 最后上传；restore 先读它
├── checkpoint.img.zstd           # gVisor 内存镜像（示例名，以实际列表为准）
├── memory0000.img.zstd           # 可能有多个 pages 文件
├── config.json.zstd              # Kata CH snapshot 成员
├── memory.zstd
├── sandboxfleet-meta.json.zstd   # Kata 辅助信息（containerID、virtiofs、网卡）
└── ...
```

**上传 / 下载规则**（对齐 `cmd/atelet` + `internal/ategcs`）：

| 步骤 | 做什么 |
|------|--------|
| 上传 | 对 `snapshotFiles` 里每个名字 `N`，上传本地 `N` → 对象 `N.zstd` |
| 下载 | 先 GET `manifest.json` → 对每个 `N` GET `N.zstd` → 解压还原为本地 `N` |
| 压缩 | zstd；源文件有 hole 时用 sparse-extent 头（与 substrate 相同 magic） |
| 删除 | 删 `manifest.json` + 列表中每个 `N.zstd` |

**为何不用 tar.gz**：substrate 按文件并行传、稀疏内存省流量；我们对齐其行为，而不是换包装。

---

## 数据结构：manifest.json（对象存储里）

对齐 substrate 的 `sandboxAssetsRecord`，SandboxFleet 版本：

```json
{
  "runtimeHandler": "runsc",
  "formatVersion": "runsc-checkpoint-v1",
  "snapshotFiles": [
    "checkpoint.img"
  ],
  "sourceSandbox": {
    "namespace": "default",
    "name": "my-agent",
    "uid": "a1b2c3..."
  },
  "poolRef": "dev-pool",
  "container": {
    "image": "busybox",
    "command": ["sleep", "infinity"]
  },
  "createdAt": "2026-08-05T12:00:00Z"
}
```

Kata 示例（`snapshotFiles` 由 Save 后扫描 checkpoint 目录得到，**不硬编码**）：

```json
{
  "runtimeHandler": "kata",
  "formatVersion": "cloud-hypervisor-snapshot-v1",
  "snapshotFiles": [
    "config.json",
    "memory",
    "sandboxfleet-meta.json"
  ],
  "sourceSandbox": { "...": "..." },
  "poolRef": "dev-pool",
  "createdAt": "2026-08-05T12:00:00Z"
}
```

| 字段 | 含义 |
|------|------|
| `snapshotFiles` | 必须下载解压的全部成员；restore **只**信这个列表 |
| `runtimeHandler` | 子 Sandbox 只能调度到同 handler 的 Worker |
| `formatVersion` | 选哪个 Load 适配器 |
| `container` | 审计/展示；**恢复不靠它冷启** |

### sandboxfleet-meta.json（Kata，作为 snapshotFiles 之一）

Save 时写入 checkpoint 目录，随其它文件一起上传为 `sandboxfleet-meta.json.zstd`：

```json
{
  "sourceSandboxID": "cri-pod-sandbox-id",
  "containerID": "cri-container-id-xxx",
  "virtiofsShares": [
    { "tag": "share0", "sharedDir": "/path/on/host" }
  ],
  "netDevices": [
    { "id": "_net1", "queuePairs": 1 }
  ],
  "savedAt": "2026-08-05T12:00:00Z"
}
```

| 字段 | 含义 |
|------|------|
| `containerID` | kata-agent Exec 用的 CRI 容器 ID |
| `virtiofsShares` | restore 时要起的 virtiofsd |
| `netDevices` | restore 时传给 CH 的网卡 id |

---

## 数据结构：Kubernetes CRD

### 1. SandboxSnapshot

```yaml
apiVersion: sandboxfleet.io/v1alpha1
kind: SandboxSnapshot
metadata:
  name: fork-20260805-abc
spec:
  sourceSandbox: my-agent
  pool: dev-pool
status:
  phase: Pending | Ready | Failed
  storagePath: snapshots/my-agent/abc/    # 对象存储路径（不是单个 tar）
  snapshotFiles:                           # 从 manifest 同步，便于 UI/删除
    - checkpoint.img
  runtime: runsc | kata
  formatVersion: runsc-checkpoint-v1 | cloud-hypervisor-snapshot-v1
  sizeBytes: 123456789                     # 逻辑总大小（各成员 uncompressed 之和）
  sourceWorker: worker-0
```

| 字段 | 含义 |
|------|------|
| `storagePath` | 对应 substrate 的 `snapshotUriPrefix` |
| `snapshotFiles` | 与 manifest 一致；删快照时按此列表删 `.zstd` |

---

### 2. Sandbox（扩展）

```yaml
spec:
  poolRef: dev-pool
  slotProfile: small
  container: ...              # 冷启动
  fromSnapshot: fork-abc        # 从快照启动（二选一）
```

---

### 3. SandboxPool（快照存哪）

```yaml
spec:
  snapshotStorage:
    s3:
      endpoint: http://minio:9000
      bucket: sandboxfleet-snapshots
      credentialsSecretRef:
        name: minio-credentials
```

---

### 4. fork API

```yaml
# 响应
snapshotName: fork-20260805-abc
children:
  - name: my-agent-child-1
```

---

## 数据结构：Worker HTTP

### CreateSnapshot

**请求**：

```json
{
  "slotID": 3,
  "identity": { "namespace": "default", "name": "my-agent", "uid": "..." },
  "storagePath": "snapshots/my-agent/abc/",
  "runtime": "runsc",
  "storage": { "bucket": "...", "endpoint": "..." }
}
```

**Worker 内部**：

```text
暂停父 → SaveSnapshot(本地 checkpointDir)
      → 扫描 snapshotFiles
      → 写 manifest.json
      → 并行上传每个 file → file.zstd（sparse zstd）
      → 上传 manifest.json
      → 恢复父
```

**响应**：

```json
{
  "storagePath": "snapshots/my-agent/abc/",
  "snapshotFiles": ["checkpoint.img"],
  "formatVersion": "runsc-checkpoint-v1",
  "runtime": "runsc",
  "sizeBytes": 123456789
}
```

---

### RestoreFromSnapshot

**请求**：

```json
{
  "slotID": 5,
  "identity": { "name": "my-agent-child-1", "uid": "..." },
  "storagePath": "snapshots/my-agent/abc/",
  "runtime": "runsc",
  "storage": { "...": "..." }
}
```

**Worker 内部**：

```text
GET manifest.json
→ 对每个 snapshotFiles[i] 下载 i.zstd 并解压到 restoreDir/i
→ LoadSnapshot(restoreDir)
→ Slot Running
```

**Slot 内部**（不暴露 API）：

| 字段 | 含义 |
|------|------|
| `restored: true` | Exec 走适配器，不走 CRI |
| `runtimeRef` | `runsc:...` 或 `kata:...` |

---

## 数据结构：Runtime 适配器（Worker 内部）

```go
SaveRequest {
  ID          // 父 CRI pod sandbox id
  DestDir     // 本地 checkpoint 目录（写出 snapshotFiles 里的那些文件）
  ContainerID // Kata：写入 sandboxfleet-meta.json
}

LoadRequest {
  SourceDir   // 下载解压后的 checkpoint 目录（不是 tar 解压）
  Identity
  SlotID
}
```

Save 完成后由 Worker（不是适配器）负责 **列举 DestDir  Regular Files → snapshotFiles → 上传**。

---

## 与 substrate 对齐总表

| | substrate | SandboxFleet |
|---|---|---|
| 控制面 | Snapshot → GCS prefix | SandboxSnapshot → S3 prefix |
| **对象布局** | `manifest.json` + `*.zstd` | **同** |
| **压缩** | sparse zstd per file | **同** |
| gVisor checkpoint/restore/exec | runsc | 同（CRI 父 + 自管子） |
| Kata checkpoint/restore/exec | CH + agent ttrpc | 同 |
| Kata OnDemand | 有 | 必做 |
| Kata restore 配网 | agent | 必做（出网） |
| gVisor restore 出网 | host 网络 | 必做 |
| 多容器 pause 模型 | 有 | 无（CRI 单容器，有意不同） |
| golden / DATA 快照 | 有 | 首期不做 |

---

## gVisor / Kata 路径（简要）

**gVisor Save**：`runsc checkpoint --leave-running` → 目录内 `checkpoint.img` 等  
**gVisor Load**：独立 root + **netns on sf-br0**（按 slot 分 `10.88.0.x`，与 Kata 对齐）+ `restore`；网内 `--network=host` 实际用的是该 netns  
**Kata Save**：CH pause/snapshot/resume + `sandboxfleet-meta.json`  
**Kata Load**：OnDemand restore + agent 配网（同 `10.88.0.0/16`）+ ttrpc Exec  

---

## fork 流程

```text
1. 父 Running
2. 预检空 Slot ≥ N
3. CreateSnapshot → storagePath 下 manifest + *.zstd → SandboxSnapshot Ready
4. N 个子 fromSnapshot → RestoreFromSnapshot → Running
5. e2e：文件一致 + guest curl 外网
```

---

## 删除 / 清理

快照会占对象存储空间，必须能主动清掉。

| 操作 | 结果 |
|------|------|
| 删子 Sandbox | 停实例、放 Slot；**不删**快照 |
| 删父 Sandbox | **不级联**删快照 / 子 |
| 删 `SandboxSnapshot` | 有子仍 `fromSnapshot` 引用 → **拒绝**；无引用 → 删 CR，并删 `manifest.json` + 全部 `*.zstd` |
| CreateSnapshot 失败 | `phase=Failed`，清半截 prefix，避免孤儿对象 |

首期不做 TTL / 自动 GC；空间靠用户显式删 `SandboxSnapshot`。

---

## 实施顺序

1. SnapshotStorage（Put/Get/Delete 多文件 zstd + manifest）  
2. Pool `snapshotStorage` + CRD SandboxSnapshot  
3. Runtime Save/Load（gVisor + Kata）  
4. Kata OnDemand + agent 配网；gVisor restore 出网  
5. Worker HTTP + Controller + fork API + **快照删除（含引用检查）**  
6. e2e：MinIO；runsc + kata；含删除与出网  

---

## e2e 验收

- fork(2) 后父仍 Running；两子占 Slot、文件一致  
- 两子 guest curl 外网成功  
- 对象存储是 `manifest.json` + 多个 `.zstd`（无 tar.gz）  
- 有子引用时删快照 → 拒绝；子删完后再删 → prefix 清空  
- CreateSnapshot 失败后无孤儿对象  
- 错误 runtimeHandler 恢复 → 拒绝  
