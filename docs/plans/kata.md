# Kata 运行时支持计划

## 1. 目标

SandboxPool 可选 Kata（Cloud Hypervisor）冷启动；Exec/Files/生命周期对齐 runc/gVisor。

首期不做：Kata Checkpoint / Restore / Fork。

## 2. 低耦合原则

| 层 | 职责 | 不做什么 |
|---|---|---|
| 集群 | `ensure-kind-cluster.sh` 按需提供 `/dev/kvm` | 不认识 kata |
| 运行时包 | `build/runtimes/kata/` 镜像 + containerd handler | 不改 Controller |
| Pool API | `cri.runtimeHandler` + 可选 `cri.hostDevices` | 不靠 handler 名猜设备 |
| Controller | 按 `hostDevices` 挂 hostPath | 无 `if kata` |
| 部署 | 薄目录：image / handler / 默认 hostDevices | 无 KVM 特判逻辑 |

## 3. 相对现状：新增什么

| 新增 | 作用 |
|---|---|
| `cri.hostDevices` | 声明 Worker 需要的宿主机设备 |
| `build/runtimes/kata/` | Kata+CLH Worker 镜像 |
| `hack/ensure-kind-cluster.sh` | 通用嵌套虚拟化（与 runtime 无关） |
| `WORKER_RUNTIME=kata` 目录项 | 选镜像/handler/默认 hostDevices |

## 4. API

```yaml
runtime:
  backend: cri
  cri:
    runtimeHandler: kata
    hostDevices: ["/dev/kvm"]   # 可选；gVisor/runc 不填
```

## 5. 流程

```text
ensure-kind-cluster（有 KVM 则挂进节点）
  → 部署 Controller + Worker 镜像
  → Pool(hostDevices=[/dev/kvm])
  → Controller 挂 /dev/kvm 进 Worker
  → CRI RunPodSandbox(handler=kata)
```

## 6. 实施顺序

1. API `hostDevices` + CRD  
2. Controller 按列表挂载  
3. `ensure-kind-cluster.sh`  
4. kata 镜像（kata-static 4.0.0 + CLH）  
5. 部署目录 + e2e/CI  

## 7. 首期不做

Live/Fork C/R · 双 VMM · Worker 用 RuntimeClass=kata · 按 handler 名特判

## 8. 决策

| 项 | 选择 |
|---|---|
| VMM | Cloud Hypervisor（configuration-clh.toml） |
| KVM | 集群能力 + Pool `hostDevices` |
| Kata 版本 | 4.0.0 |
