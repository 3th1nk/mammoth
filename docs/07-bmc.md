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
type FirmwareInventoryProvider interface { // 固件清单(纯读,v1.1 能力接口第一项,先行热身)
    FirmwareInventory(ctx, addr, cred) ([]FirmwareComponent, error)
}
type BiosSetter interface {                // BIOS 属性表读/写(Redfish Bios,高危)
    BiosAttributes(ctx, addr, cred) (map[string]any, error)             // 活表读
    SetBiosAttributes(ctx, addr, cred, attrs map[string]any) error      // 写 pending(下次生效)
}
type DriveEraser interface {               // 物理盘安全擦除(Redfish #Drive.SecureErase,最高危)
    SecureErase(ctx, addr, cred, serials []string) ([]SanitizeResult, error)
    // serials 先全部对活表解析,任一未知即整体拒绝——绝不擦一半;
    // 返回逐盘结果(厂商上报的擦除机制/时间戳,供 NIST 800-88 记录)。
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
| KVM | ⚠️ OEM 领地(Redfish 无标准资源);**OEM 映射未实装**——真机定案裸路径 URL 直开不可用,需 SSO 直链(§5、compat/huawei.md),现返回 `BMC_UNSUPPORTED` | ❌(仅 SOL 串口) |
| 硬件盘查 | ✅ Storage/Ethernet/Processor/Memory(宽容解析违规固件) | ⚠️ 有限(FRU/传感器) |
| RAID 卷管理 | ✅ `VolumeCreator`(标准载荷被拒时走 OEM 载荷,如华为 DriveID) | ❌ |
| 固件清单 | ✅ `FirmwareInventoryProvider`(UpdateService/FirmwareInventory,宽容解析,缺链接/坏条目降级) | ❌(能力缺失即无数据,盘查不失败) |
| BIOS 配置 | ✅ `BiosSetter`(Bios 属性表读 / @Redfish.Settings 设置对象 PATCH,ETag If-Match,读改写保 pending,202 任务轮询) | ❌ |
| 安全擦除 | ✅ `DriveEraser`(逐盘 `#Drive.SecureErase` 动作,serial 先全量解析再下发,202 任务轮询) | ❌ |
| 一次性引导 | ✅ | ✅ |

### 6.1 BiosSetter 两段式确认契约(高危动作范式的定稿形态)

`set_bios_attributes` 动作(v1.1 第一个写能力)确立高危动作的契约范式,
后续 Bios 助写类/擦盘类动作沿用它:

1. **请求显式确认**:请求体必须带 `"confirm": true`(ActionSetBiosAttributes),
   默认策略下缺省即 422 `BIOS_CONFIRM_REQUIRED`——提交期拒绝,不建 job;
2. **服务端二次校验**(runner 内,策略开关关不掉):重读控制器**活表**,
   不在表中的属性名整体拒绝(`BIOS_ATTRIBUTE_UNKNOWN`,带全量名单),
   与活值相同的条目计 no-op 不下发,只把真实差集作为 pending 写入
   (Redfish 语义:下次启动生效);
3. **策略开关**:`MAMMOTH_BIOS_CONFIRM=optional` 供全自动化调用方关闭
   第一道(提交侧确认标志);第二道(活表校验)永在。策略值经
   capabilities 的 `bios_set_confirm` 导出;
4. **活读面**:`GET /machines/{id}/bios` 同步返回当前属性表(console
   先例的同步 BMC 读,契约声明 502)。

### 6.2 DriveEraser 两段式确认契约(NIST 800-88 介质擦除)

`erase_drives` 动作(v1.1 第二个写能力,全契约中破坏性最高的动作)沿
§6.1 范式落盘侧 NIST 800-88 media sanitization——退役/重用途场景的数据
销毁,对象是控制器的**物理盘**(RAID 卷优先呈现时经由 PhysicalDrives
可见的成员池),不是装机语义的 wipe(那属于渲染层 wipe 档):

1. **请求显式确认**:请求体必须带 `"confirm": true`(ActionEraseDrives,
   `serials` 与 `all` 二选一),默认策略下缺省即 422
   `DRIVE_ERASE_CONFIRM_REQUIRED`——提交期拒绝,不建 job;
   `MAMMOTH_ERASE_CONFIRM=optional` 供全自动调用方关第一道(策略值经
   capabilities 的 `drive_erase_confirm` 导出);
2. **服务端二次校验**(runner 内,策略开关关不掉):重读控制器**活盘表**
   (`PhysicalDrives`),不在表中的 serial 整体拒绝
   (`DRIVE_SERIAL_UNKNOWN`,带全量名单)——解析先于擦除,绝不擦一半;
   `all=true` 展开为活表全量(serial 缺失的盘不进集合);重复 serial
   去重保序,事件与擦除序列不因请求重复而重复;
3. **驱动层**:逐盘 POST `#Drive.SecureErase` 动作 target,202 任务轮询
   (volume/bios 先例);盘未声明该动作即 BMC_UNSUPPORTED——控制器擦不了
   就必须让调用方知道(NIST 800-88 要求有方法、有记录);
4. **活读面与记录**:`GET /machines/{id}/drives` 同步返回物理盘表
   (serial 即 `erase_drives` 的身份);完成后发 `task.drive_erase` 事件,
   携带 erased serials 与厂商上报的擦除机制(method: block erase /
   crypto erase / overwrite,信息性)——即 NIST 800-88 的 sanitization
   record 底稿。实际擦除机制是厂商策略,驱动如实转述、不替控制器承诺
   Clear 还是 Purge 档;LSI 卷的 secure erase 支持度随真机窗口核验
   (见 compat/huawei.md 回归表)。

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
- **KVM URL 现状(2026-09-21)**:接口面已归一(`ConsoleURL` 驱动方法 + 同步端点,
  fake 完整可用,acceptance 覆盖);Redfish 标准无 KVM 资源,OEM 映射未实装。
  真机定案(2288H V5/iBMC 6.41):页面 URL 登录前后直开均黑屏,启动依赖 Web UI
  流程,服务侧合成 URL 原理上不可行——正解为 SSO token 直链,待二期。真机
  侦察实录见 compat/huawei.md;现统一返回 `BMC_UNSUPPORTED`,调用方按能力
  缺失降级(设计内行为,非缺陷);
- BMC 弱固件是实践中的主要不稳定源:驱动层内置请求节流、响应校验和超时隔离,
  单台 BMC 的异常不得拖垮 runner(每 target 独立超时上下文)。
