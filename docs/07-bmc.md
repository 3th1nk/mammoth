# 07 · BMC 驱动层

BMC 驱动层把异构厂商的带外管理能力归一为**统一资源模型**,供安装流水线与通用带外 API
两类消费方使用。

## 1. 统一带外资源模型

```go
type BMCDriver interface {
    Probe(ctx, addr, cred) (BMCInfo, error)      // 型号/厂商/固件/协议能力
    PowerState(ctx, addr, cred) (PowerState, error)
    SetPower(ctx, addr, cred, action PowerAction) error
        // PowerOn | PowerOff | SoftOff | HardReboot | SoftReboot | Cycle
    SetBootDevice(ctx, addr, cred, dev BootDevice, once bool) error
        // PXE | Disk | CDROM | BIOS
    MountMedia(ctx, addr, cred, img MediaImage) error     // 挂载(支持槽位多介质时逐一)
    EjectMedia(ctx, addr, cred) error
    ConsoleURL(ctx, addr, cred) (string, error)           // KVM/虚拟控制台,一次性 URL
    CollectInventory(ctx, addr, cred) (HardwareView, error)  // Redfish 盘查;IPMI 驱动返回有限集
}

// 可选能力接口(类型断言探测,不支持即降级/拒绝):
type VolumeCreator interface {            // 硬件 RAID 声明式建卷(configure_raid)
    CreateVolume(ctx, addr, cred, spec VolumeSpec) (Volume, error)  // 返回实际卷名(控制器可能改名)
    DeleteVolume(ctx, addr, cred, volumeID string) error
}
type PhysicalDriveEnumerator interface {  // RAID 成员选择需要物理盘视角(卷优先呈现时不可见)
    PhysicalDrives(ctx, addr, cred) ([]DiskView, error)
}
```

已实装的驱动侧适配(实录见 [compat/huawei.md](compat/huawei.md)):标准
`InsertMedia` 未通告时回退厂商 OEM 动作(华为 `VmmControl`,应用内重试);
Reset 拒绝时以 `ForceRestart` 重试重启类动作;自签名 TLS 经
`MAMMOTH_BMC_TLS_INSECURE` 贯穿会话与资源遍历;厂商宽容解析(违反规范的
数字类型字段)。

所有方法:

- **带超时与重试分类**:`BMC_UNREACHABLE`(网络层)/ `BMC_AUTH_FAILED`(凭证)/
  `BMC_UNSUPPORTED`(能力缺失)/ `BMC_PROTOCOL_ERROR`(厂商实现缺陷)——
  分类决定 job 层是否可重试(`retryable`);
- **并发受限**:对同一 BMC 的操作串行化(BMC 是弱硬件,并发请求会造成固件假死);
- 全部调用记审计事件。

## 2. 协议适配:Redfish 优先,IPMI 兜底

| 能力 | Redfish | IPMI |
|------|---------|------|
| 电源控制 | ✅ `ComputerSystem.Reset`(拒绝时 ForceRestart 回退) | ✅ `chassis power` |
| 引导设备 | ✅ `BootSourceOverride` | ✅ `chassis bootdev` |
| 虚拟介质 | ✅ `InsertMedia`;未通告时回退 OEM 动作(华为 VmmControl 已实装,NFS/CIFS) | ⚠️ 厂商私有(OEM 命令) |
| KVM | ✅ 厂商 OEM(归一为 URL) | ❌(仅 SOL 串口) |
| 硬件盘查 | ✅ Storage/Ethernet/Processor/Memory(宽容解析违规固件) | ⚠️ 有限(FRU/传感器) |
| RAID 卷管理 | ✅ `VolumeCreator`(标准载荷被拒时走 OEM 载荷,如华为 DriveID) | ❌ |
| 一次性引导 | ✅ | ✅ |

选择逻辑(`protocol: auto`):

```
探测 Redfish Service Root(/redfish/v1/)可达 → 使用 Redfish
  └─ 不可达/404 → 降级 IPMI
```

- 带外管理地址与凭证在 BMC 侧通常两协议通用,降级无额外配置成本;
- 虚拟介质在 IPMI-only 机型上依赖 OEM 命令,按厂商兼容矩阵声明支持度;
- 远程 URI 挂载( BMC 直接拉 URL,不落盘)与本地落盘挂载在驱动层归一,
  `MountMedia` 对调用方透明。

## 3. 通用带外 API(与安装流程解耦)

`POST /machines/{id}/actions` 的 power/boot/media/discover 动作直接落在这一层,
**不经过安装状态机**——通用带外操作是独立交付的能力(见 [01-overview.md](01-overview.md) §1),
安装只是它的消费方之一。

## 4. 厂商兼容矩阵

社区维护(`docs/compat/`),按厂商/机型声明:

| 项 | 说明 |
|----|------|
| redfish 支持 | full / partial(注明缺失资源)/ none |
| 双虚拟介质 | 是 / 否(仅单槽)/ 未知 |
| 远程 URI 挂载 | 是 / 否(必须先下载落盘) |
| 一次性引导 | 是 / 否(需恢复引导顺序补偿) |
| 已知缺陷 | 固件版本区间 + 现象 + workaround |

矩阵同时驱动运行时行为:命中"仅单槽介质"的机器自动选择单介质方案
(引导介质与发行版盘合并,渲染器生成自包含应答参数);
命中"非一次性引导"的机器在任务补偿阶段强制恢复引导顺序。

## 5. 实现注记

- 驱动层不引入厂商 SDK 依赖,Redfish 走标准 HTTP + schema 文档,IPMI 走纯协议实现;
  厂商 OEM 扩展以可选接口(`OEMExtensions`)渐进加入;
- BMC 弱固件是实践中的主要不稳定源:驱动层内置请求节流、响应校验和超时隔离,
  单台 BMC 的异常不得拖垮 runner(每 target 独立超时上下文)。
