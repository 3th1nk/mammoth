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

## 三方言回归实录(2288H V5,2026-09)

同一台机器(LogicalDrive0 卷,RAID1 3.6T)、同一 spec 形状(esp + rest、
静态单接口、MAC 钉口)连续重装三个方言,全部六阶段 succeeded:

| 方言 | 镜像 | 耗时 | 备注 |
|------|------|------|------|
| rocky9(kickstart) | Rocky 9.7 minimal(1.7G) | ~10 min | anaconda 按包装,数据量小 |
| ubuntu22(autoinstall) | live-server 22.04.4(2.1G) | ~3 h | squashfs 复制型,慢在虚拟光驱带宽 |
| debian12(preseed) | netinst 13.6(792M) | **6 min** | 修复 standard 任务集后全自动闭环 |

### 回归暴露的 rocky9 驱动缺陷(待修)

1. **grow 分区 2TiB 截断**:`part / --grow` 在该 LSI 卷上止步于扇区
   2^32-1(盘尾 1.6T 未分配),GPT 本身无此限制——疑似 blivet 在
   控制器卷几何上的 grow 分配问题。**修复方向**:渲染 grow 分区时随附
   `--maxsize=<盘容量MB>`(盘查 size_bytes 已有),显式上限不依赖
   anaconda 的分配推断;
2. **hostname 未落地**:`network --hostname` 未写入系统
   (Static hostname unset)——改 %post 直接写 /etc/hostname
   (与 debian/ubuntu 驱动的修法对齐)。

### 流程级修复(跨方言,本轮回归验证)

- **介质延迟释放**:完成回调到达时安装器可能仍在读介质(d-i 的 finish
  阶段)——eject/删文件改为 3 分钟宽限期的后台释放,不再截断收尾读取;
- **完成确认屏自动重启**:d-i 停在 "Installation complete" 等按键,
  BootParams.InstallerAutoReboot 按方言声明(anaconda/subiquity 自重启,
  d-i 否),由流水线在介质释放后补发 power cycle;
- **盘查视角一致性**:Redfish 卷名(LogicalDrive0/无 serial)与安装器
  设备名(/dev/sda+SCSI serial)不一致——目前以联调手段(盘查记录改名)
  绕过,正式机制为 early-commands 现场重识别或 ramdisk 探针(见 roadmap)。

## ubuntu22 autoinstall 端到端实录(2288H V5,iBMC 6.41)

netinst 形态的 ubuntu-22.04.4 live-server 经 BMC VmmControl 挂载 NFS 介质,
autoinstall 全自动安装,端到端跑通。过程中固化的实测结论:

### 引导与介质

- **VmmControl 挂载要求介质地址与 BMC 同网段可达**(介质 URI 指向跨网段地址时
  Connect 任务报 `iBMC.1.0.ConnectionFailed`,且失败表现滞后、伴随超时重试);
- 引导模式跟随 iBMC 全局设置(`Oem.Huawei BootType=UEFIBoot`);Redfish 的
  `BootSourceOverrideMode` 在 `Enabled: Disabled` 时**不反映**实际引导模式;
- 重装后目标机 SSH host key 重生成,登录端 `REMOTE HOST IDENTIFICATION HAS CHANGED`
  属正常(重装生命周期既有结论的又一实例)。

### ubuntu22 渲染缺陷(真机暴露,均已修复)

1. **ESP 三要素**:`flag: boot` + `grub_device: true`(分区上!)+ `fat32`——缺
   `grub_device` 即报 `autoinstall config did not create needed bootloader partition`
   (curtin 拒绝安装 bootloader);
2. **first-boot 陷阱**:`chpasswd`(user-data 顶层)与 `autoinstall.ssh.authorized-keys`
   都依赖新系统首启 cloud-init 读 seed——**新系统启动后读不到**(光盘挂载点已变),
   root 口令/公钥全部静默不生效 → 一切访问供给必须在安装期 late-commands 内完成
   (`curtin in-target`),对齐 kickstart %post 契约;
3. **完成回调用 curl 在 subiquity 环境不存在** → 改 python3/urllib;
4. **不写 `shutdown: reboot` 时 subiquity 在 curtin 完成后停滞**,不自动重启;
5. **`hostnamectl` 在 curtin chroot 内静默无效**(无 systemd)——直接写
   `/target/etc/hostname`。

### iBMC 卷名与安装器设备名的鸿沟(联调手段,机制待建)

Redfish 呈现的卷名(`LogicalDrive0`)在安装器内**不存在**(实际为 `/dev/sda`+
SCSI serial);ubuntu22 渲染在盘查无 serial 时回落 `/dev/<name>` 必然
`matched no disk`。本轮以"盘查记录改名 sda"联调跑通,正式机制二选一:
autoinstall early-commands 按 size 锚点现场重识别并改写 storage 配置
(等价 rocky9 %pre),或 PXE ramdisk 探针先行落快照(roadmap M6)。
Redfish 侧 `ID_SERIAL_SHORT` 与 `ID_SCSI_SERIAL`(lsblk SERIAL 列)是两个不同
的值,subiquity 探测取后者——serial 匹配须以实测探测语义为准。

### 安装时长与进度判断(虚拟光驱形态)

- 实测全程 ~3h(引导→curtin 完成):数据复制 ~2h(2.6GB squashfs ÷ 实测
  ~0.4MB/s)+ 本地解压/配置 ~1h;`installing system` 画面**数小时不动是常态**,
  不是卡住指示器;
- 活性/进度判断(服务端可测):`/proc/net/rpc/nfsd` 的 `proc3` 行 READ 计数
  增量=复制阶段进行中;READ 停涨+数据流归零=进入收尾(10-30 分钟内完成);
  `ra` 行计数混入元数据操作**不可用作进度**;NFS 导出上存在其他客户端时需
  tcpdump 按 host 过滤;
- 精确完成时刻以 mammoth 的 `install_reported` 事件为准(修复后的回调链路);
  subiquity 卡住时 curtin 日志不落盘,事后无法从目标盘考古精确完成时刻。

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

## ramdisk 探针原型 V0 复盘(2026-09-12,进行中)

**目标**:BMC 虚拟光驱形态的硬件采集探针——debian netinst 的 d-i 引导 +
early_command 采集(/sys 扫描,零工具依赖)+ wget 上报 + poweroff,为无 OS
机器提供内核设备名/SCSI serial(根治 Redfish 卷名与安装器设备名的鸿沟)。

**已验证**:
- VmmControl 挂载/弹出、一次性 CD 引导(ipmitool chassis bootdev cdrom)、
  探针 ISO 组装(xorriso 双条目)等组件均单独可控;
- 采集脚本设计(纯 /sys 扫描 + busybox wget 上报 + poweroff)无需任何
  工具链依赖。

**卡点(待解决)**:
- UEFI 引导结构:探针 ISO 的 isolinux(BIOS)在 UEFI 机器(BootType=
  UEFIBoot)上不被引导;补 efi.img 条目后仍未走通——引导回落到硬盘上
  UOS 半成品的 grub 命令行(前轮 Finish 崩溃的残留);
- once CD 引导与 VmmControl 挂载的**时序**(先挂后设/先设后挂)对引导
  是否生效有影响,需系统化验证;
- 34MB 探针 ISO 的 UEFI 结构(efi.img 内嵌 grub 配置与 /boot/grub/grub.cfg
  的衔接)需要本地 qemu(SeaBIOS + OVMF 双模式)快速迭代,不再真机盲试。

**下次路径**:本地 qemu 迭代探针 ISO 引导结构(BIOS/UEFI 双模式验证)→
引导成功后接上报端点(设计:token 认证的 layout 快照写入)→ discover
集成(probe=ramdisk 分支)→ 真机验证。

**正式实现的命名与生命周期**(与安装介质同构,避免并发冲突):探针 ISO 由
builder 按任务生成,命名 `probe-<token>.iso`(token 为发现任务的机器面
凭证);生成于探针触发的 prepare 阶段,上报/超时后随延迟释放删除——
与 `boot-<token>.iso` 同生命周期。V0 的手工产物(`probe-v0-manual.iso`)
仅用于链路验证,不进入任务流程。

### 显式 size 替代 --grow(已实现,真机复核 ✅)

- **发现**:anaconda 的 `--grow` 分配在该 LSI 卷上钳制于 2^32 扇区
  (`--maxsize` 亦被忽略),根分区停在 2TiB;%post 在线扩容(sfdisk +
  resize2fs)为 workaround,且 resize2fs 需在内核分区表刷新
  (partprobe/partx -u)之后;
- **根治(已实现,kickstart 渲染层)**:grow 分区改渲染**显式 size**——
  `--size = 盘查 size_bytes − 固定分区 − 512MB 余量`(全渲染期可算,与
  curtin 的显式 size 同构;多个 grow 平分预算)。预算不可信时回退
  `--grow`:无盘查容量、preserve 兄弟分区(onpart 复用块无声明尺寸)、
  未声明尺寸的兄弟分区;`--maxsize` 随之移除(其生效前提与显式 size
  相同,回退路径两者皆不可算);%post 扩容脚本保留,作为 `--grow` 回退
  路径的安全网并回收 512MB 余量;
- **已验证**:sfdisk 在线扩容对挂载中的根分区可行(手动实测 2T→3.6T);
  parted 对挂载分区直接拒绝(脚本模式警告后放弃);
- **待真机**:显式 size 在 LSI 卷上一次建对分区(3.6T 根分区,不依赖
  %post 扩容);
- **✅ 真机复核(2026-09-12 夜,job_a045f4b94988)**:六阶段 succeeded,
  `part / --fstype=ext4 --ondisk=$D0 --size=3813673` 按预期渲染($D0 动态
  解析路径),新系统实测 sda2 = 3999461785088 字节(root 满盘到盘尾
  −1MiB 对齐;anaconda 一次建对,%post 安全网回收 512MB 余量);
  装后快照自动刷新为 inband_ssh 视角(sda + 真实容量),下次重装的
  选择器/绑定直接命中设备名。

## verify_ready 的两个真机发现(2026-09-12 夜,均已修复)

1. **探活窗口竞态**:完成回调到达 → anaconda 收尾 + 重启 + POST +
   新系统 sshd,全程 2-5 分钟;而 verify_ready 的单次探活失败只吃
   任务级重试(4 次 × ~7s 退避 ≈ 30s)——本轮装完即终态失败
   (NETWORK_UNREACHABLE),机器就绪后手动 retry 才通过。
   **修复**:verify_ready 内建轮询(10s 间隔),预算
   `MAMMOTH_VERIFY_READY_WAIT`(默认 10m);认证被拒立即失败(等待
   无法愈合);真正核验前拒绝"安装器环境"会话;
2. **安装器环境假阳性**:上一轮回归中 verify_ready 在回调后 7 秒
   "成功"——物理上新系统不可能已重启完成,探活命中的是 **anaconda
   安装器环境的 sshd**(kickstart 已设 rootpw,安装器 env 放开 root
   密码登录)。**修复**:带内采集器增加 ENV 节,探测安装器运行时
   标记(/run/anaconda、/run/subiquity、/run/debian-installer、
   /lib/debian-installer);verify_ready 只把"非安装器环境"的会话
   当作新系统,安装器会话在轮询中继续等待。

另:verify_ready 曾漏传凭证的 private_key(仅 discover 路径正确),
密钥认证型凭证在装后探活必然 AUTH_FAILED——已修(b65b995)。
运维注记:**轮换 MAMMOTH_MASTER_KEY 后存量凭证全部失效**
(`cipher: message authentication failed` → CREDENTIAL_UNAVAILABLE),
须重建凭证并更新机器引用。
