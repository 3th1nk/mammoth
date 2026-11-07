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
| 硬件盘查 | ✅ Storage/Ethernet/Processor/Memory(宽容解析违规固件) | ⚠️ 有限(FRU 已实装;传感器无 SDR 栈) |
| RAID 卷管理 | ✅ `VolumeCreator`(标准载荷被拒时走 OEM 载荷,如华为 DriveID) | ❌ |
| 固件清单 | ✅ `FirmwareInventoryProvider`(UpdateService/FirmwareInventory,宽容解析,缺链接/坏条目降级) | ❌(能力缺失即无数据,盘查不失败) |
| BIOS 配置 | ✅ `BiosSetter`(Bios 属性表读 / @Redfish.Settings 设置对象 PATCH,ETag If-Match,读改写保 pending,202 任务轮询) | ❌ |
| 安全擦除 | ✅ `DriveEraser`(逐盘 `#Drive.SecureErase` 动作,serial 先全量解析再下发,202 任务轮询) | ❌ |
| 健康传感 | ✅ `HealthProvider`(Chassis→Thermal/Power 宽容 walk,overall 取 worst-of,absent 即 unknown 不拖累) | ❌(goipmi 无 SDR,缺能力即端点 422) |
| 事件日志 | ✅ `SELReader`(Managers→LogServices→Entries,跟 `Members@odata.nextLink` 翻页,倒序截断 500 条) | ❌(同上) |
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

**生效纪律(下游必须知道)**:pending 值以硬断电循环(PowerCycle)保证
应用——GracefulRestart / OS 内 reboot 不保证触发 BIOS 应用,属性停留在
pending(批量交付实战反复踩到)。装机链路天然满足(vMedia 起步即 hard
cycle);绕开装机单独下发 BIOS 的调用方,须自行跟进一次 `cycle` 再复
读 `GET /machines/{id}/bios` 活表确认落定——PATCH 2xx/任务 done 只代
表 BMC 收下了 pending,不代表属性已生效。

### 6.2 BIOS 基线模板与漂移巡检(批量下发,roadmap 下一阶段 13)

BiosSetter 单机写之上的编排面——纯既有能力组合,无新 BMC 面:

1. **基线 profile**(bios_profiles 表):命名式声明属性集(name 唯一,
   attributes 用厂商自己的名字与 JSON 类型——与 set_bios_attributes 同形);
   CRUD 面 = `POST/GET/DELETE /bios-profiles`;
2. **赋值**:`PUT /machines/{id}/bios/baseline`(body {profile_id} 赋值,
   null 清除;profile 必须存在——拼错的 id 会让后续漂移读全部落空);
   machines.bios_profile_id ON DELETE SET NULL(profile 删除不清机,
   不留悬空);
3. **漂移巡检**:`GET /machines/{id}/bios/drift` 同步读(BiosSetter 活读
   vs 基线)——**runner 写语义在读侧的同一份实现**(provision
   .CompareBiosDrift):等值计 matched,差值列 drifted([声明, 活]对,
   即批量下发的写集),活表没有的名字列 unknown(基线先于 BIOS 升级,
   或笔误出厂);无赋值 = `{"drift": null}`;每次检查落
   `machine.bios_drift_checked` 事件(档案标记 = machine 作用域的事件面,
   不设会陈旧的判定列);
4. **批量下发**:`POST /bios-profiles/{id}/apply` 扇出为**一个批量 power
   作业**(每机一个 set_bios_attributes 任务)——两段式确认契约原样适用
   (缺省 422 BIOS_CONFIRM_REQUIRED;runner 逐机活表复核,未知名整单拒);
   显式 machine_ids 或按基线赋值集,两者皆空 422。

### 6.3 DriveEraser 两段式确认契约(NIST 800-88 介质擦除)

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

### 6.4 HealthProvider / SELReader 健康面(纯读能力)

数据中心的"急救室"面:OS 崩溃、网络中断时,带外健康快照与事件日志是
仅存的观测手段(同赛道产品定位参照;DeplOS IPMI 模块调研结论)。两个
可选能力接口,固件清单同型——**纯读、无确认门禁、无持久化、无轮询**:

1. **Health**:Chassis 集合 → 每机箱 Thermal(Fans/Temperatures)+
   Power(PowerSupplies/Voltages)宽容 walk,传感器带
   ok|warning|critical|unknown 四态;overall 取 chassis Status 与全部
   传感器的 worst-of——absent/unreadable 一律 unknown,**不拖累**整体
   (宽容解析纪律:一份不可读的描述符绝不发明一条告警);风扇
   ReadingUnits 缺省回填 RPM;
2. **SELReader**:Managers → LogServices → Entries 连接遍历,日志集合
   按追加序(老→新)到达,截取集合尾部再取条目体(链接一次 GET,内容
   至多 `SELMaxEntries`=500 次),按 created 倒序 + id 稳定排序;severity
   归一到契约词表,"Debug"/OEM 串归 unknown 而非默默升格为 OK;
3. **活读端点**:`GET /machines/{id}/health`、`GET /machines/{id}/sel`,
   console/bios 同步读先例(422 缺能力 / 502 BMC 错);IPMI 不实现
   (goipmi 命令栈无 SDR/SEL,先例:固件清单——真机全走 redfish);
4. **边界**:周期带外巡检(异常→事件→webhook)是有状态服务化能力,
   等真实需求触发再排(roadmap 第 9 项);本节只交付按需读。

### 6.5 boot_rescue 救援动作(内存修复系统,非破坏性)

商业裸金属云的标配能力(内存 RAM OS + SSH 修复),自研引擎普遍缺失;
mammoth 全件复用:alpine agent 内存系统(bootstrap/plan 机制)+ 虚拟介质
载体 + token 化 render 面 + syslog sink。**非破坏性,不进两段式确认门**
——但它重开机并独占引导链,与 install 同答 JOB_MACHINE_BUSY 互斥门
(409,双向:install 拦 rescue,rescue 拦 install,查询面 flow IN)。

1. **动作形态**:`POST /machines/{id}/actions`
   `{"type":"boot_rescue","ssh_keys":[...]}`——ssh_keys 可选,缺省 =
   console-only 救援(根 shell 走 BMC KVM/SOL,BMC 访问本身就是门);
2. **载体**(双载体,装机通路同款分型):virtual_media 缺省(alpine
   standard ISO 重打包,agent overlay + rescue 计划在介质根,dd 同构);
   `boot:"pxe"` 显式覆盖(vMedia 固件死路机型的逃生门);**protocol=none
   机器强制 PXE**(headlessActionGate 对 boot_rescue 放开——无介质槽,
   PXE 是 headless 唯一能拿到的救援,arm 走 headless 证据路径:带内重启
   / WoL / 人工上电);extended ISO /apks 池两载体共用
   (`MAMMOTH_WINDOWS_APPLY_ALPINE_ISO`;PXE 载体另需
   `MAMMOTH_PROBE_ALPINE_NETBOOT` 引导树)——池缺席诚实降级 console-only
   (双侧告警,不失败);
3. **执行流**:单阶段 rescue flow(建载体 → vMedia 挂载一次性引导 →
   等 agent 的 rescue-ready 报告 → 释放载荷);agent 侧
   `MAMMOTH_RESCUE=1` 分支:apk openssh → authorized_keys 注入 →
   sshd → report ok → **前台 console shell(不 reboot)**——救援会话
   长驻,日志随报心跳;报告走与 install 相同的
   `/render/{token}/complete` 面(handler 按 flow 分流);
4. **终态语义**:rescue-ready 即任务成功 + 载荷即刻释放(vMedia 弹载 +
   引导序回 disk)——机器**留在内存系统里**,运维修完后任意 power 动作
   重启即回原系统;超时(默认随探针预算 10 分钟)按探针语义关机释放,
   重试整段重建(幂等);
5. **事件**:`task.rescue_reported`(报告面)/ `task.boot_rescue`
   (ready 落档,带 ssh_keys 数)/ `task.stage_changed`(armed);
6. **边界**:机器状态机不动(rescue 不改 state);救援不修数据——内存
   系统里能做什么由运维决定,引擎只负责把机器送进可修复状态。


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
- **KVM URL 现状(2026-11-06 更新)**:接口面已归一(`ConsoleURL` 驱动方法 + 同步端点,
  fake 完整可用,acceptance 覆盖);Redfish 标准无 KVM 资源,OEM 映射未实装。
  真机定案(2288H V5/iBMC 6.41):页面 URL 直开黑屏的机制级根因已解——启动
  状态载体是 BMC origin 的 localStorage 一次性标志(H5ClientModel),不在
  URL,服务侧合成 URL 与 SSO 直链两条路同灭;**正解 = console 侧反代 +
  浏览器驱动启动序列**(engine 持凭证维护 GUI 会话,详见 compat/huawei.md
  §KVM 集成方案)。现统一返回 `BMC_UNSUPPORTED`,调用方按能力缺失降级
  (设计内行为,非缺陷);
- BMC 弱固件是实践中的主要不稳定源:驱动层内置请求节流、响应校验和超时隔离,
  单台 BMC 的异常不得拖垮 runner(每 target 独立超时上下文)。
