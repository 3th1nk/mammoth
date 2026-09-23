# 厂商实录:H3C HDM(文档推演,待真机复核)

> 本文档与 [huawei.md](huawei.md) 不同:全部内容摘自官方文档
> **《H3C HDM Redfish 参考手册》(资料版本 V3.35,适用 HDM-3.26 及以上)**
> 与既有驱动的对号推演,**尚未经任何真机验证**。矩阵 YAML(known_defects
> 需可复现固件区间)在真机复核前保持为空——本页只回答"接上 H3C 时
> 该预期什么、先验什么"。
>
> 跨厂商共性模式见 [patterns.md](patterns.md);新厂商到货 30 分钟预检
> 流程见 [README.md](README.md)。

## 底座判定:AMI MegaRAC(文档实证)

手册会话管理章节给出的响应头示例:

```
Server: AMI MegaRAC Redfish Service ...
```

HDM 是 **AMI MegaRAC 底座**的又一个 OEM 实例——这是兼容矩阵 AMI MegaRAC
行(此前零样本)的第一份文档证据。这意味着 [huawei.md](huawei.md) 的
iBMC(同为 AMI 系)上验证过的模式有先验适用性,最显眼的一条已经命中:

- **存储集合为 `/Systems/{id}/Storages`(非标准复数)**——HDM 与 iBMC
  同形。gofish 按通告链接遍历可走通,已有宽容解析无需改动。

## 文档基线

| 项 | 值 |
|----|----|
| 适用固件 | HDM 3.26+(资料版本 V3.35) |
| Redfish 版本 | 1.9.0(服务根 `RedfishVersion`) |
| 会话并发上限 | 10(超出拒绝新建) |
| 会话超时 | SessionTimeout 默认 1800s(可配 30~86400) |
| 鉴权 | Basic / X-Auth-Token 会话,**标准载荷**(无 OEM 域) |

与华为的关键反差:**标准 `{"UserName","Password"}` 会话载荷在 HDM 上直接
可用**(iBMC 必拒,需 `Oem.Huawei.Domain`)。驱动的 `createSession` 按服务
根 Oem 字段识别厂商,非华为走标准载荷——H3C 预期零改动命中。

## 与既有防御的对号(逐条预期)

### 1. 会话(预期零改动)

标准载荷 + `X-Auth-Token`。上限 10 个并发、默认 30 分钟超时:驱动的
会话缓存(addr+user,TTL 5min,认证失败失效)本来就把并发压到每机 1 个,
远低于上限;但 **HDM 会话超时上限 86400s、下限 30s** 提醒:若现场把
SessionTimeout 调到 30s,5min TTL 缓存会持有过期 token——复用失败时
驱动已有"认证失败即失效重建"路径,预期自愈。

### 2. If-Match ETag 全覆盖(预期零改动)

HDM 对**所有 PATCH** 要求携带资源当前 ETag(`If-Match`),与 iBMC 的
Bios/Settings 路径一致但范围更大。驱动 `patchSystemBoot` 已按
"资源 ETag 原样透传为 If-Match"实现,BIOS 属性写同样带活读 ETag——
预期两个写路径都命中。真机复核点:ETag 在 HDM 上是否也会在会话间
漂移(iBMC 实测"资源标注上的 ETag 会过期,须 PATCH 时活读")。

### 3. 电源动作:ResetType 全集(预期零改动,能力更宽)

HDM 通告完整集合:`On / ForceOff / ForceRestart / GracefulShutdown /
Nmi / ForcePowerCycle`。驱动的"预读 allowed values + 拒绝即回退"逻辑
是按通告防御的,全集厂商回退永不触发;**Nmi 在 HDM 文档化可用**——
mammoth 的 PowerAction 枚举暂无 nmi 动作,记为潜在扩展而非适配项。

### 4. 一次性引导:三参数同发 + 两类约束(预期零改动,复核点明确)

- **约束一**:HDM-2.26 起 `BootSourceOverrideMode` 与
  `BootSourceOverrideTarget` **不得同时为 None**(否则拒);
- **约束二**:`BiosSetup` 目标仅在 `Enabled=Once` 时合法;
- **请求形状**:Enabled / Mode / Target 三个属性**必须同一请求内一起发**,
  单独 PATCH 其一被拒。

驱动的 `SetBootDevice` 本来就单请求整发三属性,H3C 预期直接命中。
目标集合:`None / Pxe / Hdd / Cd / BiosSetup`——无 `Utilities`(UEFI
shell)等扩展目标。一次性语义(Once)按文档标准,是否像 iBMC 一样
"安装器进引导后自动回退 None/Disabled"待真机复核。

### 5. 虚拟介质:OEM VmmControl,`/Oem/Public/` 命名空间(已适配,驱动层)

HDM 与 iBMC 同样**不依赖标准 `#VirtualMedia.InsertMedia`**,提供 OEM 动作:

```
POST .../VirtualMedia/{id}/Oem/Public/Actions/VirtualMedia.VmmControl
{"VmmControlType": "Connect", "Image": "nfs://<server>/<export>/<file>.iso"}
```

与华为 VmmControl **载荷逐字段同形**(VmmControlType + Image),差异全在
OEM 命名空间:`/Oem/Huawei/` → **`/Oem/Public/`**。文档化行为:

- `Connect` → **202 + Task**,任务态经 **`Mounting`** 等在途态后终态
  (见 §6——这正是本轮任务终态判定的修复动机);
- `Disconnect` → **同步 200**(无任务);
- 协议:**仅 NFS / CIFS**,与 iBMC 同;http(s) 直链预期被拒(具体错误码待真机);
- 镜像文件名 ≤128 字符(超出拒绝);
- 挂载前是否需要先 Disconnect(iBMC 的 `ConnectionOccupied` 语义)文档未提,
  驱动固定"先断后连"对 HDM 无害,保留。

驱动适配:`vmm.go` 的动作 URL 不再硬编码 `/Oem/Huawei/`——优先读槽位
资源 `Actions["#VirtualMedia.VmmControl"].target`(通告什么用什么),
未通告时按候选序 `/Oem/Huawei/` → `/Oem/Public/` 逐个试探。**部署要求
同华为:机器可达的 NFS(或 CIFS)服务导出镜像目录**。

### 6. 任务状态字典:HDM 的在途态比规范多(已适配,驱动层)

HDM 文档化的 TaskState 全集:`New / Running / Uploading / Verifying /
Updating / Downloading / Mounting / Waiting For Effect / Going To Effect /
Completed / Failed / Cancelled / Killed / Exception / Mounted`。

关键教训:**`Mounting`(挂载中)是在途态**——白名单式判定
(`!= Running && != New` 即视为终态)会把它误判为终态,造成"挂载尚未
落定就报成功"的静默失败(iBMC 一次性引导教训的 HDM 版本:`过早弹出 =
CD 引导失败落回旧系统`)。**已修复**:全驱动统一改为终态黑名单
(`taskTerminal`),只把 {`Completed, Killed, Exception, Cancelled, Failed,
Mounted, Waiting For Effect, Going To Effect`} 判为终态,**未知状态一律
按在途轮询至 deadline**——未知终态只烧轮询预算(安全),未知在途被
当终态是过早成功(不安全),两害取其轻。

### 7. BIOS 属性写(预期低改动命中,待真机复核)

文档路径与 iBMC 同形:`/Systems/{id}/Bios/Settings` + If-Match + 200 同步
返回 pending,下次启动生效。差异点(待复核):

- BIOS 属性变更任务是否也走 202 + 任务(iBMC 是 200 同步);
- `ChangePassword` 动作**无需 If-Match**,204 返回;
- `ResetBios` 动作恢复默认。

驱动的 BiosSetter(活读属性表 → PATCH Settings → ETag 透传)预期直接命中。

### 8. RAID 卷管理:Oem 载荷 + 200 成功信封(仅记录,不适配)

HDM 文档化的建卷走 OEM 载荷(与华为 `Oem.Huawei.VolumeRaidLevel` 不同形):

```
POST /Systems/1/Storages/{id}/Volumes
{"Oem": {"Level": "RAID 0", "SpanNum": 1, "NumDrives": 2,
  "PhysicalDiskList": [{"group_id": 0, "id": <ConnectionID>}]}}
```

- 成功响应是 **200 + error 信封携带 `Base.1.0.Success`**(非 202 + 任务)——
  与 Redfish 主流的 202+Task 相反,现有 `CreateVolume` 的任务轮询不适用;
- `id` 对应盘资源的 ConnectionID(华为是 `Oem.Huawei.DriveID` 整数);
- 删卷:DELETE 卷资源,标准。

**本轮不实现**(第二套 OEM 建卷方言,等真机需求再落);真机接入时若
需要 RAID,预检清单里先验此形状。

### 9. KVM 深链(ConsoleURL 候选,待真机复核)

HDM 提供 `POST /Systems/1/kvm` 获取 **H5 KVM 令牌**(`H5_KVM_Authority`),
配合 Web 前缀拼出 HTML5 控制台直链。与 huawei.md 的结论(URL 直开
黑屏、SSO token 是正解)呼应但机制不同——H3C 的令牌交换是文档化的
API,理论上 ConsoleURL 可实现为"POST 取 token → 拼深链"。真机复核前
保持 `BMC_UNSUPPORTED`。

## Roadmap 候选(文档可见、mammoth 暂无对应能力)

- **配置导入/导出**:HDM 支持整机配置的导出与导入(202 + 任务)——
  对应"装机前基线化"场景;
- **告警订阅/推送**(告警上报策略、SNMP Trap/邮件):对应流水线外的
  硬件健康监控;
- **固件升级**:组件固件镜像上传 + 升级任务(在途态 `Uploading/
  Verifying/Updating` 即为此设计)——BMC 固件治理场景;
- **存储介质告警水线**:卷/物理盘的预警阈值设置;
- **许可证管理**:HDM 功能许可的导入。

以上均不在本轮范围,列出只为接真机时有对号表。

## 待真机预检清单(30 分钟,对齐 README 预检流程)

1. **会话**:标准载荷 POST /SessionService/Sessions → 记录响应头
   `Server`(核对 AMI MegaRAC)与 `X-Auth-Token`;
2. **盘查**:跑一次 `discover`(Redfish 全量)→ 重点核 `/Systems/1/Storages`
   路径是否按文档、盘拓扑(Drives/Volumes)是否像 iBMC 一样可能为空
   (coverage: partial 预期);
3. **引导**:`set_boot_device(Cd, once=true)` → 立即 GET Boot 回读
   (Enabled/Mode/Target 三值正确?)→ 是否出现 iBMC 式"Pxe 静默跳过"
   需 BIOS 前置的问题;
4. **电源**:`soft_reboot` / `hard_reboot` 各一发(验证 ResetType 全集
   通告下回退逻辑不误触发);
5. **虚拟介质**:NFS 导出 ISO → `mount_media` → **盯任务态序列**
   (预期出现 `Mounting`,验证终态判定修复)→ `Inserted: true` 回读;
   弹出一次(Disconnect 同步 200?);
6. **ETag**:PATCH Boot 后重 GET,核对 ETag 是否变化(修 §2 复核点);
7. **卷**(如机器有 RAID 卡):按 §8 手工 POST 一次,取证 200+Success
   信封形状;
8. **KVM**:POST /Systems/1/kvm 取 token,核对深链形态(§9)。

预检全绿后:按 README 矩阵格式补 `h3c.yaml`(partial_inventory/
single_virtual_media 等以实测为准,known_defects 需固件区间),本页的
"待真机复核"逐条回填实测结论。
