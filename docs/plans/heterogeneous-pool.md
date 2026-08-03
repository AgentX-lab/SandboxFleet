# 异构 SandboxPool 实施计划

## 目标

- Pool 内多种 WorkerTemplate；Worker 内多种 SlotProfile。
- Sandbox 按 `slotProfile` 匹配；首版随机调度，策略可替换。
- **只提供 Slot / Worker 的扩容与缩容**；不提供就地改 CPU/内存等资源限制。
- 前提：Worker 大概率长期有业务，**不能依赖「整 Worker 全空闲再滚 Pod」作为扩容主路径**。
- Runtime 仍为 Pool 级配置。

## 模型

```text
SandboxPool
├── SlotProfile: small / large          # 规格创建后固定
├── WorkerTemplate: mixed × 2
│   └── 每 Worker: small × 2 + large × 1
└── WorkerTemplate: large-only × 1
    └── 每 Worker: large × 2
```

- **SlotProfile**：固定资源规格（创建后不改配额）。
- **WorkerTemplate**：`replicas` + Slot 组成（`profile` + `count`）。
- **Worker**：按模板创建的 Pod；Pod 资源 = 创建时 Slot 配额之和。
- **SlotID**：Assignment 与 Worker API 的稳定定位键。

## API

### SandboxPool

```yaml
spec:
  runtime:
    backend: cri
    cri:
      runtimeHandler: runsc
  slotProfiles:
    - name: small
      resources: { requests: {cpu: 100m, memory: 128Mi}, limits: {cpu: 200m, memory: 256Mi} }
    - name: large
      resources: { requests: {cpu: "1", memory: 1Gi}, limits: {cpu: "2", memory: 2Gi} }
  workerTemplates:
    - name: mixed
      replicas: 2
      slots:
        - { profile: small, count: 2 }
        - { profile: large, count: 1 }
```

规则：

- Profile / Template 名唯一；`count`、`replicas` > 0；SlotGroup 必须引用已有 Profile。
- Worker Pod 资源 = 该模板展开后全部 Slot 资源之和（创建/滚动时一次定死）。
- **不可变**：`runtime`、Profile/Template **名称**、Profile **`resources`**。
- **可变（仅扩缩容）**：
  - `replicas`：Worker 台数
  - `slots[].count`：每 Worker 上某 Profile 的 Slot 数量

需要更大/更小规格时：新增另一种 Profile + 对应 Slot，或删空闲旧 Slot；**不改已有 Profile 的 resources**。

空闲与容量校验由 Pool Controller 做；CEL 只做结构合法性。

### Sandbox

```yaml
spec:
  poolRef: my-pool
  slotProfile: large   # 不可变
  container: { image: example/image }
status:
  assignment: { worker, slotID, slotProfile }
```

### Status

- 汇总：`currentWorkers` / `readyWorkers` / `usedSlots` / `availableSlots`
- 分项：每 Template 副本数；每 Profile 总量 / 已用 / 可用
- `Updating`：扩缩容因占用或容量不足暂未完全生效

调度库存只认 Worker 实际上报。

## 扩缩容（唯一变更面）

原则：不改配额；不破坏占用中的 Slot / Assignment；不靠「忙 Worker 原地胀 Pod」。

### Worker 扩缩容（`replicas`）

- **扩容**：增加 ordinal，新 Pod 按当前模板 Slot 布局与资源创建。
- **缩容**：只删最高序号、且其上全部 Slot 空闲的 Worker；有占用则保留，置 `Updating=True`，稍后重试。

这是**加大集群容量的主路径**（常忙场景下优先加 Worker，而不是改现有 Worker 配额）。

### Slot 扩缩容（`slots[].count`）

SlotID 是身份，不是必须连续的下标。

```text
原：0=small, 1=small, 2=large

允许：
- 删任意空闲 Slot（如删 1）-> 0=small, 2=large
- 新增时优先复用最小空闲 ID（补洞）；无空洞时用 maxID+1

禁止：
- 改已有 Slot 的资源限制
- 给仍存在的 Slot 改号 / 交换 Profile
- 删被占用的 Slot
```

#### 缩容（减少 count）

- 只移除空闲 Slot（按 Profile 保留最低 ID）。
- 更新 ConfigMap；Worker 热加载去掉空闲 Slot。
- Pod 外壳资源**不强制缩小**（可偏大，安全）；不因此重启忙 Worker。

#### 扩容（增加 count）

现有 Worker 常忙，**禁止**「先改 ConfigMap 让 Slot 变多、Pod limits 仍旧」这种超卖。

规则：

1. 计算新增 Slot 所需资源。
2. **仅当**现有 Worker Pod 当前已申请资源仍能覆盖「新旧 Slot 总和」时，才允许在该模板上加 Slot（例如先前删过 Slot，外壳有富余）。
3. 否则：**拒绝在本模板原地加 Slot**，应通过提高 `replicas`（或新 Template）扩容；Status `Updating` / Message 说明容量不足。
4. 不把「等 Worker 全空闲再滚 Pod 涨额度」当作默认扩容手段。

落地要点：

1. Controller 对比 desired / observed Slot 集合（按 ID）。
2. 删：目标 ID 空闲才写 ConfigMap。
3. 加：通过「外壳剩余额度」校验才写 ConfigMap；STS Pod template 资源与创建时一致或仅在新 Worker 上体现更大布局。
4. 不满足则保留旧拓扑，`Updating=True`，条件满足后重试。

## Worker / Runtime

```go
type SlotSpec struct {
    ID        int32
    Profile   string
    Resources corev1.ResourceRequirements // 来自 Profile，创建后不变
}
```

- ConfigMap 挂载展开后的 `[]SlotSpec`；Worker 不理解 Template。
- `CreateRequest` 带本 Slot 资源；CRI 按请求设限。
- `ListSlots` 回报 Profile / Resources / State。
- 支持加载**拓扑变更**后的 ConfigMap（增删 Slot）；不支持改已有 Slot 的 Resources。

## Pool Controller

1. 每 Template 一个 StatefulSet + Slot ConfigMap；共用 Headless Service。
2. 聚合容量与 Template / Profile status，同步 Scheduler。
3. reconcile 只处理 replicas 与 Slot count 扩缩；Profile.resources 变更视为非法 / 忽略并告警（API 层 immutable）。
4. Worker 缩容：目标 Pod 全空闲才删。
5. Slot 扩容：受现有 Pod 资源包络约束；不够则引导加 replicas。

## 调度

```go
type Strategy interface {
    Name() string
    Select(ctx context.Context, req AssignRequest, candidates []Candidate) (Candidate, error)
}
```

- Core：库存、硬过滤（健康 / 空闲 / Profile）、锁内占用、Restore / Release。
- 首版 `RandomStrategy`；后续策略只注册，不改 Controller。
- 流程：过滤候选 → Strategy 选择 → 写 Assignment → Reserve/Start。
- 未生效的拓扑不进调度；以 Worker 上报为准。

失败要点：

- 无效 Profile → `InvalidSlotProfile`
- 无空闲匹配 → `NoCapacity`
- Start 暂失败 → 保留 Assignment，幂等重试
- 扩缩容被占用或额度不足阻塞 → `Updating=True`

## 实施顺序

异构主体已有。按本简化模型收敛：

1. API：`SlotProfile.resources` 标为不可变；去掉「改配额就地生效」相关设计与测试预期。
2. Controller：保留 Slot 删（空闲）+ 补洞新增；新增时做 **Pod 资源包络校验**。
3. 删除 / 停用：因改配额触发的全 Profile 空闲校验、忙 Worker 为涨额度而滚动重启等逻辑。
4. Worker：`ApplySlots` 仅允许增删与同 ID 规格不变；若 Resources 被改则拒绝。
5. `Updating` 覆盖：缩容遇忙、扩容额度不足。
6. 单测 / E2E：扩 replicas、缩空闲 Worker、删空闲 Slot、补洞、额度不足拒绝加 Slot。

## 验收

- 异构创建、按 Profile 调度、随机策略可测、状态聚合正确。
- **不提供**改 Profile/Worker/Slot 资源限制的就地更新。
- 删任意空闲 Slot 成功；删占用失败。
- 在外壳额度足够时加 Slot 成功并补洞；额度不足时不超卖，提示用 replicas 扩容。
- replicas 扩容立即加 Worker；缩容不删占用 Worker。
- 常忙场景下加容量的正确方式是加 Worker，而不是等全空闲改配额。
- 单元测试与 E2E 通过。
