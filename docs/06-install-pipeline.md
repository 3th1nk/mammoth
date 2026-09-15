# 06 · 安装流水线

从 Install Spec 到一台就绪的机器,流水线分六个 stage(三方言均已真机闭环,
实现状态见各节与 [compat/distros.md](compat/distros.md)):

```
verify_layout → configure_raid → prepare_media → boot → install_os → verify_ready
```

## 1. verify_layout(布局校验)

- 校验 spec 中所有 `select` 能在目标机器 `hardware` 上解析出唯一盘;
  多义时结合 `root_device_hints` 消歧,仍多义则任务失败(`SELECT_AMBIGUOUS`);
  `size: largest/smallest` 在其余过滤条件的**结果池内**取极值;
- `keep: partitions` 声明与最新 layout 快照比对:盘存在、分区号存在;
  preserve[] 条目绑定当前快照,注入为保留分区(携带快照事实);
- 该阶段在提交侧(schema 校验)与执行侧(双重)各做一次——提交侧给用户即时反馈,
  执行侧防提交后机器状态变化。

## 2. configure_raid(RAID 声明式配置)

`storage.raid[]` 的独立可重入 stage(六阶段相对五阶段的增量即此):

- **hardware**:经 Redfish Volume 创建(`bmc.VolumeCreator` 可选能力,redfish 实装/
  fake 脚本化/ipmi 不支持)→ 重扫库存 → 卷名绑定逻辑盘(卷名以控制器实际命名为准,
  幂等按"成员序列号集合 + RAID 级别"判定);
- **software**:不在此 stage 落盘,由渲染层产出 anaconda raid 行(成员全盘 raid
  分区 `--grow`,每卷单挂载点;多分区需 LVM,后续);
- 成员选择器与 `disks[]` 共用同一解析器,重叠即 `SCHEMA_INVALID_STORAGE`。

## 3. prepare_media(载荷准备,builder 面执行)

prepare_media 的后半段按**引导策略(boot strategy)**分派——"把安装器送到机器
面前"是策略,前半段的渲染是策略无关的纯函数。策略在提交时声明
(`spec.boot.strategy`,缺省取部署默认 `MAMMOTH_BOOT_STRATEGY`),task context
记录 `boot_strategy` 标记,所有清理路径按标记分派。

### 3.1 virtual_media 策略:发行版原盘重打包为任务引导介质(默认)

发行版原盘只读引用、永不修改(仍是包源);builder 从原盘**产出本任务的引导 ISO**
(`boot-<token>.iso`,token 为机器面凭证),应答文件与安装器内核参数全部烘入:

```
发行版 ISO(仓库只读引用)
  └─ builder 探测形态 → 三条装配路径:
     casper(Ubuntu live-server)/ d-i(Debian netinst,探测 /install.amd)→ 全量重打
     RHEL 系 isolinux 树(Rocky)→ 选择性装配(引导位 + 内核/initrd + 应答文件)
  └─ 注入:内核参数(inst.ks= / ds=nocloud-net;s=file:///cdrom/ / file=/cdrom/preseed.cfg)
     + 应答文件烘入 ISO 根(含 run/mammoth/*.sh 脚本)
```

- **离线 seed 是真机结论**:安装器早期组网拉远程 seed 已证不可靠(casper 早期
  组网失败、d-i 完全无网络),seed 必须取**本轮渲染的产物**烘入;
- 安装期包装配仍走原盘:RHEL 系经 `url`/`nfs` 命令或 `inst.repo` 指向 NFS/HTTP
  上的原盘 ISO(anaconda 自动 loop 挂载);debian netinst 从重打包内自带 pool 安装;
- 介质与任务解耦:`boot-<token>.iso` 可缓存、并发、重试;完成回调后经 3 分钟
  宽限期后台释放(安装器收尾可能仍在读介质——真机教训)。

> 设计原则:**介质与机器解耦**。介质按 task-token 命名而非按机器,批量场景
> 同 spec 机器可共享介质缓存;单机制定的部分只有烘入的 seed(渲染产物)。

### 3.2 应答文件渲染

渲染是纯函数:`render(spec × machine view) → answer files + boot params`。

- 输入输出全部持久化(spec 快照 + 渲染产物),失败可离线复盘;
- 渲染层不做任何外部调用(不查快照、不连 BMC),保证可测试性与确定性;
- 模板按发行版方言组织,由发行版驱动提供(见 §6)。

### 3.3 pxe 策略:网络引导(proxyDHCP + iPXE,M7)

> 场景与全链路导览(两条入门路径对比、引导接力链、跨网段/Relay)见
> [11-pxe-walkthrough.md](11-pxe-walkthrough.md);本节是机制与实现约定。

借鉴 Pixiecore 的 proxyDHCP 模型:**mammoth 永不分配地址**(站点 DHCP 拥有
地址权),只在旁路应答 PXE 客户端"下一级引导程序在哪"。链路与职责:

```
PXE ROM ──DHCP DISCOVER(opt 60=PXEClient)──▶ proxyDHCP(UDP 67/4011,旁路应答)
        ◀──OFFER: yiaddr=0 + opt66/67(NBP 文件名,TFTP 服务器=mammoth)
PXE ROM ──TFTP RRQ──▶ undionly.kpxe(BIOS)/ ipxe-<arch>.efi(UEFI,go:embed 内置)
iPXE    ──HTTP GET /netboot/script?mac=…──▶ 按 MAC 渲染的 iPXE 脚本
iPXE    ──HTTP GET /netboot/files/<token>/…──▶ kernel/initrd(+modloop)
内核    ──inst.ks=<ExternalURL>/render/<token>/ks.cfg + inst.repo=nfs:…──▶ 安装
```

- **载荷构建**(`builder.ExtractBootFiles`):从发行版原盘提取引导文件
  (kernel/initrd/alpine modloop,与 ISO 装配共用同一份"各形态引导文件在哪"
  的事实),归一化为平面文件名落 `MediaDir/netboot/<token>/`;
- **引导项注册**:机器库存里的**全部 NIC MAC**(叠加 spec 网络声明的
  match.mac)写入 `netboot_entries`(`UNIQUE(mac)`,prepare 重试幂等覆盖);
  固件从哪个口引导是固件的事,全注册;
- **无条目回退**:脚本端点对未知 MAC 返回 200 + `exit` 脚本(绝不 404——
  404 会把 iPXE 留在自己的 shell;`exit` 回固件引导序自然落盘),顺带无害化
  "安装后固件再次 PXE"的竞态。**零注册入门(已实现)**:`MAMMOTH_PXE_ENROLL`
  开启时,未知 MAC 改渲染 **enroll 脚本**——引导进共享探针树
  (`MediaDir/netboot/enroll`,启动时构建一次,`MAMMOTH_PROBE_ALPINE_NETBOOT`
  为载体),`enroll_mac=<mac>` 走内核参数、共享 overlay 运行时读
  /proc/cmdline 拼进上报 URL,`POST /netboot/enroll/{token}` 落
  `pending_machines` 台账(见 docs/05-inventory.md §4 与
  docs/08-data-model.md);
- **kernel args 与 ISO 通路零差异**:`inst.ks=` 本就是绝对 HTTP URL、
  `inst.repo=nfs:` 本就是网络安装源、无静态网声明时 earlynet 自动 `ip=dhcp`
  ——RHEL 系是唯一零新增安装源工作的家族(矩阵见 §6);
- **ramdisk 探针同链路**:`probe=ramdisk` + `boot=pxe` 时,探针环境以同一
  引导通路进入内存(overlay 以第二段 cpio 追加进 initramfs,modloop 走
  HTTP),见 docs/05-inventory.md §4;
- **补偿**:注册/注销与介质生命周期同构(完成回调 → 3 分钟宽限 → 注销 +
  删引导树;终态失败/取消/重试 → 即时注销),孤儿由 reaper 按任务终态清扫;
- **部署前提**(docs/operations.md):机器 L2 可达、UDP 67/69/4011 开放、
  Secure Boot 关(iPXE 未签名;shim+grubnet 列后续)、单 L2 单应答者、
  `MAMMOTH_PXE_NEXT_SERVER`(ExternalURL host 是 IP 字面量时自动派生)。

## 4. boot(引导)

按 prepare_media 选定的策略分派(接口 `bootStrategy{prepare,arm,release}`):

- **virtual_media**:BMC 挂载 `boot-<token>.iso` + 设置一次性引导
  (`boot_device once`);经中转分发的部署可配 `MAMMOTH_BOOT_SETTLE_DELAY`
  (挂载与上电之间的等待——BMC 挂载校验只读镜像头部,残缺镜像会静默引导
  失败);
- **pxe**:`set_boot_device(BootPXE, once)` + 上电——无挂载、无 settle
  (引导树已在本地盘,不存在带外传输尾巴);
- 断电→上电(或 reset),进入安装环境;
3. 等待策略:等待安装器的完成回调(`POST /render/{token}/complete`);
   自重启安装器(anaconda/subiquity)自行重启,d-i 停在完成确认屏,由流水线在
   介质释放后补发 power cycle(`BootParams.InstallerAutoReboot` 按方言声明);
4. 介质在完成回调到达后延迟释放(见 §3.1),早于安装器自身重启的单次引导
   一次性已消费,不会二次进入安装介质。

## 5. install_os(安装执行)

安装环境内,%pre / early-command 钩子执行 Mammoth 注入的校验-生成脚本
(动机:无头装机中应答文件的磁盘声明错误表现为 anaconda **静默回退交互
模式**、d-i **卡死在对应步骤且无报错**——均不可远程诊断;%pre 强校验把
这类失败前移为显式错误码退出):

```
1. 读取实际分区表(lsblk/blkid/sysfs)
2. 与任务绑定的快照基线比对(声明了 preserve / drift check 时):
   - preserve 声明的盘/分区是否存在、sysfs start/size 扇区与 blkid UUID 是否一致
   不一致 → 回报 LAYOUT_DRIFT + 终止安装(安装器以显式错误码退出)
3. 一致 → 按意图动态生成最终分区动作:
   wipe 盘 → 清表重建
   keep: disk → 不触碰
   preserve 分区 → 不格式化,--onpart 按原 uuid 挂载
   其余 → 重建
4. 写出最终应答文件片段,交由安装器执行
```

- **Redfish 卷名与安装器设备名的鸿沟**在此消解:非内核命名(LogicalDriveN)的
  目标盘,part 行携带 `$Dn` 占位符,%pre 按 size±1% + serial 现场解析生成动态
  `%include`(rocky9;装后快照刷新让下一轮直接命中设备名,见 05 §5);
- **这是"保留数据分区"的安全兜底:任何预采集都可能过期,执行现场校验使
  "按过期快照装错盘"在机制上不可能发生。**

安装完成后的核验(verify_ready):

- post_install 脚本阶段执行用户注册类脚本,末尾回调完成端点(status=ok);
- 带内探活确认新系统可达(内建轮询,见 §7 verify_ready 行);配置了带内凭证的
  机器随后刷新 layout 快照(装后视角:设备名 + serial 即安装器所见);
- 恢复引导顺序。

## 6. 发行版驱动(OS Driver)

新增发行版 = 新增一个驱动实现 + 模板,**零编排层改动**:

```go
type OSDriver interface {
    Distro() string                     // rocky9 / ubuntu22 / debian12 / uniontechos
    SupportedArchs() []Arch
    // 一次渲染同时产出应答文件与该方言的引导参数(inst.ks= / ds=nocloud / file=):
    RenderAnswers(in InstallInputs, m MachineView) ([]AnswerFile, BootParams, error)
    KeepPartitionSupport() SupportLevel // full | partial | none
}
```

- 驱动注册制:注册表按 `spec.image.distro` 路由(现注册 rocky9、uniontechos、
  ubuntu22、debian12);
- `KeepPartitionSupport()` 进入发行版支持矩阵(`GET /api/v1` 导出),**不支持分区级
  保留的发行版在提交时即拒绝 `keep: partitions`**,而不是装到一半失败;
- 可选能力 `PXEDriver{PXESupport() SupportLevel}`:声明网络引导支持级
  (未实现的驱动按 none),`boot.strategy=pxe` 的提交按此门禁
  (`SCHEMA_UNSUPPORTED_BOOT_STRATEGY`),与 keep 门禁同型;
- 可选能力 `FirmwareDriver{FirmwareSupport() FirmwareSupport}`:声明媒体
  可引导的固件范围(缺省 all;rocky10 = uefi_only,上游已移除 BIOS 引导
  镜像)。提交链按机器的 option 93 观测值
  (`machines.pxe_firmware`,docs/08)逐台门禁——UEFI-only 媒体派给 BIOS
  固件机器即 `SCHEMA_FIRMWARE_MISMATCH`,该 task 创建即 failed、不阻塞
  同批兄弟机器;**无观测不门禁**(从未在引导线上出现过的机器不受影响,
  观测是时点事实而非当前 BMC 引导模式的承诺);
- 应答文件名归驱动(rocky9:ks.cfg;ubuntu22:user-data;debian12:preseed.cfg +
  脚本),编排层不硬编码。

### 发行版支持矩阵(实现态;运行时事实源 `GET /api/v1`)

| 发行版 | 安装器 | 应答文件 | 保留分区 | PXE | 状态 |
|--------|--------|---------|---------|-----|------|
| RHEL 系(Rocky/Alma) | Anaconda | kickstart | **full**(`%pre` + `--onpart/--noformat`) | full(`inst.repo=nfs:` 网络装机现成;rocky10 BIOS 待真机) | ✅ 真机闭环(Huawei 2288H V5) |
| Ubuntu Server 22.04 | subiquity | autoinstall | partial(keep: disk) | none(casper 需整 ISO 进内存) | ✅ 真机闭环 |
| Debian 12 | debian-installer | preseed | partial(keep: disk) | none(d-i 需网络镜像源) | ✅ 真机闭环 |
| 统信服务器 V20(UOS) | anaconda 定制 | kickstart(同 rocky9 方言) | full(同 rocky9) | full(同 rocky9) | **blocked**(Finish 阶段崩溃,见 distros.md) |
| Windows | Setup | unattend | full | 未开始 | 未开始 |

各方言能力差异(bond/vlan/软件 RAID/多安装盘/xfs 等)见
[compat/distros.md](compat/distros.md) 实现注记与
[04-install-spec.md](04-install-spec.md) §5。

## 7. 幂等与重试语义

| stage | 重试行为 |
|-------|---------|
| verify_layout | 纯读,直接重跑 |
| configure_raid | 幂等:卷按"成员序列号 + 级别"判定已存在即复用 |
| prepare_media | 渲染产物与介质按 task-token 幂等(已存在即复用) |
| boot | 重新挂载介质、重设引导并重启;若机器已在安装中,由 attempt 计数与 deadline 判定是否中断重装 |
| install_os | 以 `%pre` 校验为安全边界:重装前快照漂移会被拦截 |
| verify_ready | 纯读;带内探活内建轮询(预算 `MAMMOTH_VERIFY_READY_WAIT`,默认 10m)——覆盖重启+POST 窗口;安装器环境(anaconda/subiquity/d-i 标记)不作为核验对象 |

`ForceRetry`(跳过失败 stage 强行续跑)仅限 `verify_ready`;其余 stage 的失败必须
从该 stage 重跑——不存在"跳过校验"的选项。
