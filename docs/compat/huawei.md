# 厂商实录:Huawei iBMC(2288H V5,iBMC 6.41)

> 2026-09 真机实测记录(第一台接入的真实硬件)。驱动层的适配已合入代码;
> 本页是兼容矩阵的"现场报告"形态,供其他用户对照。

## 实测环境

| 项 | 值 |
|----|----|
| 机型 | FusionServer / 2288H V5 |
| iBMC 固件 | 6.41(Redfish 1.0.2) |
| 处理器 | 1 × Xeon Silver 4110(8 核) |
| 内存 | 32 GiB |

## 发现与适配

### 1. Session 需要 OEM 字段(已适配,驱动层)

标准 Redfish 会话载荷(`UserName`/`Password`)在 iBMC 上**必拒**(`iBMC.1.0.AuthorizationFailed`),
WebUI 实际发送的载荷带华为 OEM 域:

```json
{"UserName":"...","Password":"...","Oem":{"Huawei":{"Domain":"LocaliBMC"}}}
```

驱动修复:`redfish.createSession` 读取免鉴权的服务根,凭 `Oem.Huawei` 识别厂商,
会话 POST 携带 OEM 域;会话建立后以 `X-Auth-Token` + `Location` 交给 gofish
预认证继续资源遍历。其他厂商走标准载荷。

### 2. 自签名证书(已适配,配置层)

iBMC 默认自签名证书;`MAMMOTH_BMC_TLS_INSECURE=true` 覆盖会话握手与
gofish 资源遍历两处传输。

### 3. ProcessorId/Socket 类型违规(已适配,驱动层宽容解析)

iBMC 的 `ProcessorId.EffectiveFamily` 与 `Socket` 输出**数字**,
Redfish 规范为字符串——严格解析器(gofish)将整个处理器集合判废。
驱动改用宽容解析器:只取 `Model`/`TotalCores`,逐条求和 socket 核数;
`ProcessorSummary` 在该固件上不可信(`Count=1`、`Model="Central Processor"`),仅作回退。

### 4. 盘拓扑不暴露(已知盲区,coverage: partial)

`/Systems/1/Storages`(注意:非标准复数形式,但 gofish 按通告链接可走通)下
RAIDStorage0 控制器可见,但 **Drives 与 Volumes 均为空**——iBMC 6.41 不在
Redfish 上枚举盘拓扑(与 AVAGO 背板管理配置相关)。盘查结果标注
`coverage: partial`、注记 `no drives or volumes reported`,规格级盘查仍完整
(身份/处理器/内存/网卡)。

### 5. 网卡命名(已适配,驱动层)

`EthernetInterface.Name` 为泛化的 "System Ethernet Interface";端口身份在
`Id`(`mainboardLOMPort1`)。驱动改用 `Id` 作为 NIC name,并过滤无 MAC 的
虚拟接口。

## 实测盘查结果(经 mammoth 真实流水线)

```json
{
  "state": "ready",
  "power_state": "on",
  "bmc": {"vendor": "huawei", "model": "2288H V5", "firmware_version": "6.41"},
  "hardware": {
    "serial_number": "2102312…",
    "cpu": {"model": "Intel(R) Xeon(R) Silver 4110 CPU @ 2.10GHz", "cores": 8},
    "memory_bytes": 34359738368,
    "nics": [
      {"name": "mainboardLOMPort1", "mac": "50:1D:93:…"},
      {"name": "mainboardLOMPort2", "mac": "50:1D:93:…"}
    ],
    "coverage": "partial",
    "coverage_notes": ["no drives or volumes reported"]
  }
}
```

### 6. 虚拟介质:OEM VmmControl 动作,仅支持 NFS/CIFS(实测)

`/Managers/1/VirtualMedia` 下有 CD 与 USBStick 两个资源,但标准
`#VirtualMedia.InsertMedia` 动作**未通告**(Actions 为空)。真正的通路是
华为 OEM 动作:

```
POST /redfish/v1/Managers/1/VirtualMedia/CD/Oem/Huawei/Actions/VirtualMedia.VmmControl
{"Image": "nfs://<server>/<export>/<file>.iso", "VmmControlType": "Connect"}
```

- `Connect` 返回 202 + Redfish Task(轮询至终态;`Exception` 时读 Messages);
- **`http://` URI 被直接拒绝**:`iBMC.1.0.FileTransferProtocolMismatch`
  ("image URI 与动作参数中的传输协议不匹配")——即该固件的 VmmControl
  仅支持 NFS/CIFS 类共享,不支持 HTTP 直链;
- `Disconnect` 为同步弹出(实测:`eject_media` 动作 → 任务 succeeded →
  Redfish `Inserted: False`),契约已补 `eject_media` 动作类型;
- 连接失败回报 `iBMC.1.0.ConnectionFailed`;
- **Connect 是概率性失败,重试即成功(实机抓包定论)**:对同一 NFS URI,
  一次 Connect 3 秒即 Exception,紧接着的 Connect 却完整走通——tcpdump 显示
  成功路径上 BMC 的 NFSv3 客户端完整完成 portmapper/mountd/NFS 握手、
  lookup/getattr 后开始读镜像;失败路径在握手完成后即放弃。任务消息自带的
  Resolution 就是 "Please try again"。驱动侧 `MountMedia` 对 Connect 做
  应用内重试(≤3 次,间隔 4s),避免烧掉流水线任务级重试;
- **成功挂载耗时 ~12-15s**:握手 <100ms 完成,中间 ~13s 是 BMC 校验
  ISO9660 元数据(读 PVD/目录记录),随后任务 Completed + `Inserted: true`。
  操作超时预算需覆盖这一段(MAMMOTH_BMC_TIMEOUT 建议 ≥60s);
- Connect 前必须先 Disconnect:槽位被占时直接 Connect 报
  `iBMC.1.0.ConnectionOccupied`。驱动固定"先断后连"(断开失败静默忽略)。

驱动修复:`redfish.MountMedia/EjectMedia` 在标准动作未通告时回退到
VmmControl(任务轮询至终态,失败分类为 BMC_PROTOCOL_ERROR 并携带 iBMC
消息)。**部署要求:机器可访问的 NFS 服务导出镜像目录**(也可用 CIFS)。

**✅ 实测挂载成功**:`mount_media` + `nfs://198.51.100.248/data/os_iso/.../Rocky-9.7-x86_64-minimal.iso`
→ VmmControl Connect → 任务 succeeded → Redfish `Inserted: true`、`Image` 指向该 NFS URI、
`MediaTypes: ["CD"]`。注意 URI 路径必须精确到 NFS 导出目录下的真实文件
(HTTP 根目录与 NFS 导出目录是两个不同路径,别混淆)。

**对安装流水线的影响**:此机型上 image.source 与介质地址应使用
`nfs://host/export/xxx.iso` 形态;引导介质注入(kickstart)在该机型上
仍受 KVM 通路限制(见 §6 上述),完整安装需 NFS 介质 + PXE/KVM 组合。

### 7. 一次性引导(实测)

`BootSourceOverrideTarget=Cd + Enabled=Once` 设置成功,重启进入安装器后
**自动回退**(`Target: None / Enabled: Disabled`)——一次性语义在真机正确,
无需驱动侧补偿(07-bmc.md §4 的"非一次性引导补偿"在此机型不需要)。

### 8. 软重启(实测)

`GracefulRestart` 与 `PowerCycle` 在此固件/电源状态下均被拒
(`ActionParameterValueFormatError`)——`ForceRestart`(hard_reboot)可用。
驱动侧已加回退:Reset 拒绝且属于重启类 ResetType 时,自动以 ForceRestart
重试一次(安装流水线的 boot 阶段用 power cycle 引导进安装器,即靠此回退)。

### 9. 其它已核实形状

- 存储集合链接为 `/Systems/1/Storages`(非标准复数);gofish 按通告链接可走通;
- `/Systems/1/Processors` 集合本身可用,仅单条目(本机 1 颗物理 CPU);
- 网卡 `Id` 为真实端口身份(`mainboardLOMPort1/2`),`SpeedMbps`/`LinkStatus` 不上报。

## 端到端重装实录(2026-09-10,六阶段全通过)

在此机型上完成首次真实硬件端到端重装(Rocky 9.7,UEFI,硬件 RAID 逻辑盘,
NFS ISO 装包源,静态网络)。全链路打通过程中固化下来的事实:

1. **存储拓扑可变性**:同一台机器,盘查结果会随 RAID 卡配置在
   "物理盘直通"与"逻辑盘(LD0/LD1,coverage: full)"之间切换。逻辑盘名
   (`LogicalDrive0/1`)不是安装器内核设备名(sda/sdb)——rocky9 驱动对
   非 kernel 命名的 wipe 盘自动走 %pre 现场解析(按 size±1% + serial 匹配
   lsblk,生成动态 %include 承载 clearpart/part/bootloader)。
2. **UEFI 要素**(BootSourceOverrideMode=UEFI):
   - 引导介质必须同时喂饱两种平台:isolinux 相对路径(syslinux 以配置
     目录为基准)+ grub 绝对路径;内核/initrd 需在两个目录各有一份;
   - kickstart 必须声明 ESP(`part /boot/efi --fstype=efi`),否则
     anaconda 报 "failed to find a suitable stage1 device" 中止;
3. **安装源形态**:anaconda 的 `url` 命令只收 http(s)/ftp 的解包树;
   NFS 上的 ISO 用 `nfs --server= --dir=<ISO所在目录>`(kickstart 命令)
   或 `inst.repo=nfs:server:/path/file.iso`(内核参数,冒号必须有),
   anaconda 检测 .iso 自动 loop 挂载。
4. **无 DHCP 的数据面**:安装器拉取远程 kickstart 前不配网——rocky9 驱动
   从 spec.network 渲染早期网络内核参数(`ifname=m0:<mac> ip=...::gw:mask::m0:none`,
   按 MAC 钉死网口,与安装器内命名无关);无静态配置时回退 `ip=dhcp`。
   kickstart 内 network 命令经 %pre 按 MAC 解析真实接口名(含网关)。
5. **eject 时机**:boot 阶段挂载的介质必须保持到安装器上报完成——
   服务器 POST 自检期间固件才读介质,过早弹出 = CD 引导失败落回旧系统。
6. **root 密码与 SSH**:Rocky 9 的 sshd 默认 `PermitRootLogin
   prohibit-password`,生成的 root 密码仅控制台可登;SSH 进新系统需
   在 spec.scripts 里自行放开或改用密钥。

## 硬件 RAID 卷管理(实测,iBMC 6.41 + AVAGO)

- **物理盘**暴露于 `/Chassis/{id}/Drives`(SerialNumber 齐全),而 Storage
  资源的内联 Drives 数组额外携带 `Oem.Huawei.DriveID`(整数)——**建卷载荷
  要的就是这个整数**:`POST /Storages/{id}/Volumes`,
  `{"Name": ..., "Oem": {"Huawei": {"VolumeRaidLevel": "RAID1", "Drives": [0, 2]}}}`
  → 202 + 任务,`VolumeCreationSuccess`。标准载荷(RAIDType 属性)被拒
  (PropertyUnknown);盘被已有卷占用时报 `DriveStatusNotSupported`;
- **DELETE** 卷资源可用(标准),同样 202 + 任务;删卷后成员盘转为
  UnconfiguredGood,重新可建卷(UnconfiguredBad 的盘需先手工 make good);
- **卷名被控制器忽略**:无论请求什么 Name,卷都叫 LogicalDriveN(自动
  递增)——驱动 CreateVolume 返回"实际出现在盘查里的卷名",流水线按返回
  值绑定;幂等语义按"成员序列号集合 + RAID 级别"判定,而非卷名;
- **盘查呈现规则与 RAID 意图的关系**:卷优先呈现时物理盘不可见,而 RAID
  成员选择恰恰需要物理盘 → 新增可选能力 `PhysicalDrives`(bmc.PhysicalDriveEnumerator)
  供 verify_layout 解析硬件 RAID 成员;同时物理盘的协议名归一为 "raid"。
- 驱动侧成员映射:内联数组的 Id(如 HDDPlaneDisk0)↔ 单盘资源里的
  SerialNumber,两段拼出 serial→DriveID 的映射。

## 介质服务形态

1. **内置导出(默认)**:mammoth 进程内建只读 NFSv3 导出(go-nfs,单端口
   2049 多路复用 mount+nfs),BMC 直接挂 `nfs://<mammoth主机>/` 下的
   MediaDir——零上传、零外部依赖。已用 Linux v3 客户端验证互操作;
   iBMC 客户端的挂载验证待真机轮(注意 go-nfs 不提供 portmapper/rpcbind,
   若 iBMC 的 mount 客户端强依赖 111 端口查询则需补一个 mini-portmapper)。
2. **外部 NFS(大规模生产)**:`MAMMOTH_NFS_EXPORT=false` 关闭内置导出,
   `MAMMOTH_MEDIA_BASE_URI` 指向客户的 NFS 服务;若 mammoth 主机对其无写
   权限,配合 `MAMMOTH_MEDIA_RELAY_*`(SSH 中转,原子可见)。
3. **内核 nfsd**:通用 NAS/大规模场景的最优解,由客户基础设施承担,
   mammoth 不内嵌(内核模块与特权要求不适合默认形态)。

## 介质中转竞态(部署注意)

经中转(本地构建后 scp 推 NFS)分发引导介质时,**挂载成功 ≠ 文件完整**:
iBMC 的挂载校验只读镜像头部,部分文件即可通过;固件在 POST 后期才真正
读取内核/initrd,读到残缺数据即 CD 引导失败、静默落回旧系统盘(无任何
报错回流)。两项缓解,均已实现:
1. 推送侧原子可见(先传临时名再改名);
2. `MAMMOTH_BOOT_SETTLE_DELAY`(boot 阶段挂载与上电之间的等待,默认 0;
   中转分发部署建议 ≥ 推送耗时)。
