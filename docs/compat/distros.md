# 发行版支持矩阵(distro support matrix)

运行时事实源是 `GET /api/v1` 的 `distros` 字段(驱动注册表实时导出);
本页是与之同步的人读版与各发行版的实现注记。语义见
[06-install-pipeline.md](../06-install-pipeline.md) §5。

| 发行版 | 驱动 | 安装器 | 应答文件 | 保留分区 | 网络稳定选择器 | 状态 |
|--------|------|--------|----------|----------|----------------|------|
| Rocky/RHEL 系(Rocky 9/Alma 9) | `rocky9` | Anaconda | kickstart(`inst.ks=`) | **full**:`%pre` 漂移守卫 + `--onpart/--noformat` | MAC → 接口名在 `%pre` 安装期解析 | ✅ v0.1 |
| **Rocky 10 / RHEL 10 系** | `rocky10` | Anaconda(**UEFI-only**,上游移除 Legacy BIOS) | kickstart(同 `rocky9` 方言) | **full**(同 `rocky9`,机制同源) | MAC → 接口名在 `%pre` 安装期解析 | ✅ 真机闭环(2288H) |
| **CentOS 7** | `centos7` | Anaconda 19.31(python2 世代) | kickstart(同 `rocky9` 方言) | **full**(同 `rocky9`) | MAC → 接口名在 `%pre` 安装期解析 | ✅ 真机闭环(2288H;大盘需独立 /boot,见注记) |
| **银河麒麟 V11**(Server V11 2503) | `kylinv11` | Anaconda(现代代际) | kickstart(同 `rocky9` 方言) | **full**(同 `rocky9`,机制同源) | MAC → 接口名在 `%pre` 安装期解析 | ✅ 真机闭环(2288H;中文 NFS 路径全链路验证) |
| **银河麒麟 V10**(Server V10 SP3 2403) | `kylinv10` | Anaconda(RHEL8 代际 + NM 1.18) | kickstart(同 `rocky9` 方言) | **full**(同 `rocky9`) | MAC(渲染层带 NM 修复段) | ⚠️ 受限:静态网络自动化卡死在 NM(见注记);**中文 NFS 路径经 anaconda 层已验证可用** |
| Ubuntu Server 22.04 | `ubuntu22` | Subiquity | autoinstall(nocloud seed) | **partial**:`keep: disk` 可用;`keep: partitions/preserve` 提交即拒绝 | netplan `match.macaddress` 原生支持 | ✅ v0.3 |
| Debian 12 | `debian12` | debian-installer | preseed(`file=/cdrom/preseed.cfg`) | **partial**:`keep: disk` 可用;`keep: partitions/preserve` 提交即拒绝 | 无(netcfg 不按 MAC 选口,单接口) | ✅ 真机跑通 |
| 统信服务器 V20(UOS) | `uniontechos` | **anaconda 定制**(RHEL 系安装树:AppStream/BaseOS/isolinux,非 d-i) | kickstart(同 `rocky9` 方言) | **full**(同 `rocky9`,待真机复核) | MAC → 接口名在 %pre 安装期解析 | **blocked**(见下) |
| Windows | — | Setup | unattend | full(目标) | — | 未开始 |

## 保留分区支持语义(SupportLevel)

| 级别 | 提交侧行为 | 渲染侧行为 |
|------|-----------|-----------|
| `full` | keep: disk / keep: partitions / preserve 全部受理(仍受快照命中校验) | 完整渲染 + `%pre` 漂移守卫 |
| `partial` | keep: disk 受理;keep: partitions / preserve → `SCHEMA_UNSUPPORTED_KEEP` 拒绝 | keep: disk 以"不进入存储配置"实现;渲染层二次拒绝 keep: partitions |
| `none` | 一切 keep → `SCHEMA_UNSUPPORTED_KEEP` 拒绝 | — |

`GET /api/v1` 返回示例:

```json
{
  "version": "1.0.0",
  "resources": ["credentials", "machines", "jobs", "tasks", "layout", "events"],
  "distros": [
    {"name": "rocky9",   "keep_partition_support": "full"},
    {"name": "rocky10",  "keep_partition_support": "full"},
    {"name": "centos7",  "keep_partition_support": "full"},
    {"name": "kylinv10", "keep_partition_support": "full"},
    {"name": "kylinv11", "keep_partition_support": "full"},
    {"name": "ubuntu22", "keep_partition_support": "partial"},
    {"name": "debian12", "keep_partition_support": "partial"},
    {"name": "uniontechos", "keep_partition_support": "full"}
  ]
}
```

## 实现注记

### rocky9(kickstart)

- 应答文件:`ks.cfg`(task-token URL 经 `inst.ks=` 注入);
- 网络:`%pre` 钩子运行时解析 MAC → 接口名,生成 `network` 行(bond slaves 按 MAC);
- 保留分区:`%pre` 漂移守卫逐分区比对 sysfs start/size 扇区与 blkid UUID,
  漂移即回报 `LAYOUT_DRIFT` 并终止安装;preserve 分区 `--onpart --noformat`;
- 完成回报:`%post` 末尾 curl 完成回调。

### rocky10(kickstart,UEFI-only)

- **介质形态**:RHEL10 上游移除 Legacy BIOS——ISO 无 isolinux,El Torito 的
  BIOS 项为占位(`images/eltorito.img`),UEFI 走 appended ESP 分区;
  `builder` 新增 rhel10 布局族:**全量重打包**(as_mkisofs 的 interval 引用
  源 ISO,构建时源在本机即可复现),grub 条目用原生 `linuxefi/initrdefi`,
  卷标原样保留(efiboot.img 的 grub 按卷标搜索配置);
- **BIOS 机器不可装**(上游立场一致):该机型只有 UEFI 固件时无影响;
- EnsureISO:`nfs://` 等 URI 的路径在本机文件系统存在时直接使用
  (单机部署 mammoth 与 NFS 导出同机的零拷贝路径);
- 其余(kickstart 语法/keep/网络)与 rocky9 同源。

### centos7(kickstart,老 anaconda)

anaconda 19.31(python2 世代)+ util-linux 2.23 的四处方言差异,均已内建:

- **grub 命令**:UEFI 的 grub 2.02 无 `linux` 命令——selective 布局的
  grub.cfg 统一用 `linuxefi/initrdefi`(Rocky 9/10 原生配置同款,
  BIOS 链的 isolinux.cfg 不受影响);
- **syslinux 模块**:syslinux 4.x 无 `.c32` 模块(isolinux.bin 自含 ldlinux),
  选择性提取时 `.c32` 为可选件,缺失即跳过;
- **hostname**:`network --hostname=` 不渲染(解析器世代风险,且 rocky9
  真机已证不可靠),主机名由无条件的 %post `/etc/hostname` 直写承载;
- **根分区在线扩容安全网不渲染**(`RootExtensionSupported=false`):
  util-linux 2.23 的 sfdisk 不能操作 GPT,一段注定失败的脚本会把装完的
  系统拖成 fatal——渲染期显式 size 已精确建满盘,512MB 余量留在盘尾
  (无害);
- **大盘布局要求(提交侧注意)**:>2TiB 卷上 blivet 强制 boot 文件系统
  位于前 2TiB——`/` 直接占满整盘会被拒(storage checks 退交互),
  **需声明独立 /boot(1G 级)+ / rest**;ESP 用 `--fstype=efi`(渲染层
  已自动映射)。

### kylin(银河麒麟服务器)

**V11 2503(✅ 真机闭环,2288H)**:现代代际 anaconda + 新版 NM,走标准路径
(early `ip=` 内核参数 + MAC 解析接口名,零 workaround),六阶段一次闭环。
**中文 NFS 路径全链路验证通过**:`inst.repo` 内核参数与 kickstart `nfs`
命令中的 UTF-8 中文目录(grub/isolinux 配置透传 → dracut → anaconda NFS
挂载)均正常,安装源正确识别——中文目录本身不构成障碍。

**V10 SP3 2403(⚠️ 静态网络自动化受限)**:V10 的 NM 为 1.18 世代,对
"initramfs 已配置过的设备"打 `platform-init` 硬性 unmanaged 标记
(`nmcli device set managed yes` 无法覆盖),而其 payload 又以 NM 状态为
网络判据——**死锁**:自动安装永远停在 text 概要的 Network spoke。
渲染层的 netRepair 段(%pre 修复 anaconda 写坏的 ifcfg:`HWADDR50:…` 缺
`=`、UUID 段错位,再重新托管)不足以解除该标记。裸 `ip=dhcp` 回退则死在
更早:dracut 阶段挂中文 NFS 路径失败(emergency shell)。
**当前交付形态**:V10 SP3 需一次 TTY2 干预(按上序修 ifcfg 后
`nmcli con reload && nmcli device connect <iface>`,回装界面按 b);
或使用 V11 镜像(推荐,全链路验证)。

### ubuntu22(autoinstall)

- 应答文件:nocloud seed(`meta-data` + `user-data`,烘入 ISO 根,经
  `ds=nocloud-net;s=file:///cdrom/` 离线加载——远程 seed 依赖 casper 早期组网,
  真机已证不可靠);
- 网络:netplan(cloud-init network-config v2),bond slaves 用 `match.macaddress`;
- 存储:curtin storage config(disk→partition→format→mount 链);
  keep: disk = 该盘不进入 curtin 配置;
- 完成回报:`late-commands` 末尾 **python3/urllib** POST 回调(subiquity 环境
  无 curl/wget);
- **一切访问供给在安装期完成**(真机教训:first-boot cloud-init 读不到 seed,
  静默失效)——root 口令(chpasswd)、authorized_keys、PermitRootLogin、
  hostname 全部经 `late-commands`/`curtin in-target` 落盘;
- 显式 `shutdown: reboot`——否则 subiquity 在 curtin 完成后停滞不重启;
- root 口令:留空 = 安装期随机生成(经任务事件一次性下发)。

### debian12(preseed)

- 应答文件:`preseed.cfg` + `run/mammoth/{pre,post}-install.sh`,全部烘入 ISO 根,
  经 `file=/cdrom/preseed.cfg` 离线加载(d-i 将引导介质挂在 /cdrom);
  语言/键盘问题发生在 preseed 加载**之前**,以内核参数形式早期预置
  (`debian-installer/locale` / `keyboard-configuration/layoutcode`——d-i 内核参数
  天然是 preseed 键值);
- 介质形态:netinst 必须全量重打(d-i 从 ISO 的 pool//dists/ 读包),builder 探测
  `/install.amd`(兼容 `/install`)后走 casper 同款全量重打路径;
- 存储:`partman-auto/expert_recipe`(声明分区顺序即落盘顺序,esp → `method{ efi }`,
  swap → `method{ swap }`,grow → max=10⁹ MB 由 partman 截断);
  keep: disk = 该盘不成为 `partman-auto/disk` 目标;
- **方言限制(渲染期拒绝,表现为 RENDER_FAILED)**:bond/vlan/多条静态(netcfg 单接口
  且不按 MAC 选口)、软件 RAID(partman md 配方未接)、多安装目标盘(partman-auto 单盘)、
  xfs(netinst 是否携带 partman-xfs 未验证,白名单 ext2/3/4、vfat/fat32、swap);
- 完成回报:`preseed/late_command` 执行 `/cdrom/run/mammoth/post-install.sh`,
  busybox `wget --post-data` 上报(d-i 环境无 curl;**部署需 http**,busybox wget 的
  TLS 受限);pre_install 挂 EXIT failtrap,失败即回报阶段名;
- root 口令:`passwd/root-password` 明文(与 kickstart/ubuntu22 同语义,一次性随机);
- 注意:UOS Server V20(1050a)经 ISO 实测为 **anaconda 定制安装器**
  (RHEL 系安装树),已改归 kickstart 方言(见 `uniontechos` 行与
  docs/compat/huawei.md),不在本 preseed 包内。

### ubuntu d-i(legacy) 支持决策:不主动支持

**决策(2026-09)**:不为 ubuntu 的 debian-installer 镜像(18.04 server ISO /
mini.iso)提供驱动支持。

- **上游已封存**:Ubuntu Server 自 20.04 起弃用 d-i,不再发布 d-i 版 ISO,
  mini.iso 构建同样停更;最后一个版本 18.04 标准支持已于 2023-05 EOL
  (仅 ESM 延续至 2028)。
- **20.04+ 无对应镜像**:ubuntu 批量装机的加速正道是 PXE + live(网络拉
  squashfs,较虚拟光驱快一个量级,roadmap M6),而非回退 d-i;ubuntu 方向
  优先投入 24.04 兼容性验证(subiquity/autoinstall v1 主线,预计现有驱动
  直接可用)。
- **重评估触发条件**:存量 18.04 机器出现批量纳管/重装需求时。

**如需适配的技术方向**(低成本,预计 ~1 天 + 一轮真机验证):

- **方言复用**:18.04 的 d-i 与 debian 12 同源——preseed 键、partman
  expert_recipe、`preseed/late_command`(busybox wget 回调)基本通用,即
  `debian.New("ubuntu1804")` 级别的变体注册,无需新驱动;
- **布局探测**:ubuntu d-i ISO 的内核目录为 `/install`(非 install.amd),
  `builder.debianInstallDir` 的候选列表已包含 ✅;注意 ubuntu live-server
  也含 `/install/`,探测顺序必须保持在 casper 之后(现有实现已如此);
- **需核对的差异点**:ubuntu 仓库/apt-setup 键的措辞差异、18.04 d-i 版本的
  partman 组件行为、ESM 源(若目标机依赖)的 mirror 配置;
- **风险**:绑定一个上游停止演进的安装器,后续无人修复——变体需在文档与
  capabilities 中如实标注支持边界。

### uniontechos 状态:blocked(UOS 定制 anaconda)

- **现象**:全自动 kickstart(安装树/包/分区/网络全部正确,489 包安装完成)下,
  UOS 定制 anaconda(33.16.4.15)在 Finish 阶段崩溃:
  `dasbus.error.DBusError: max() arg is an empty sequence`(task_proxy.Finish);
- **已排除**:`bootloader --location=mbr`(去除后同样崩溃)、`eula --agreed` +
  `user` 注入(同样崩溃)——崩溃源在 UOS 定制 anaconda 的 Finish 任务组内部,
  kickstart 参数层无法绕过;
- **恢复路径**:拿 anaconda-tb 深层帧定位空任务组所属的 UOS 定制模块
  (需 UOS 官方支持或 anaconda 定制源码),或等待 UOS 新版修复;
- 引导/介质/包装配等其余链路均正常(包安装完成、只差 Finish 收尾)。

### 新增发行版

实现 `render.OSDriver`(六个方法)+ 注册一行,编排层零改动:
`Registry.For(distro)` 路由、`KeepPartitionSupport()` 进入提交门禁与矩阵导出。

### PXE 网络引导(M7,boot.strategy=pxe)

| 发行版 | PXE 支持级 | 说明 |
|--------|-----------|------|
| rocky9 / centos7 / kylinv10 / kylinv11 / uniontechos | full | anaconda/dracut 内核对网络引导原生;`inst.repo=nfs:` 安装源复用 nfsx 导出,`inst.ks=` 走 HTTP,与 ISO 通路零差异 |
| rocky10(UEFI-only 媒体) | full | 引导文件同为 `images/pxeboot/*`,UEFI 侧无虞;BIOS 引导上游已移除(镜像 UEFI-only),不做 BIOS PXE。驱动声明 `FirmwareUEFIOnly`,派给观测为 BIOS 固件的机器在提交期即被 `SCHEMA_FIRMWARE_MISMATCH` 拒绝(不阻塞同批其他机器) |
| ubuntu22 | none | casper 需把整张 ISO 拉进内存或 http root,需新的安装源策略后再排期;实测参考:低内存机器(4GB 级)整盘载入会 tmpfs 写满而失败——PXE 化设计须走 kernel+initrd+网络源,不做整 ISO 进内存 |
| debian12 | none | d-i netboot 后包必须走网络镜像源,破坏离线安装语义,同上 |

实现注记:

- 引导项按机器**全部 NIC MAC** 注册(spec 网络声明的 match.mac 叠加);
  固件从哪个口引导属固件行为;
- 引导链:undionly.kpxe(BIOS,依赖网卡 UNDI)/ UEFI x64 走 shim+grubnet
  (shimx64.efi → grubx64.efi,Microsoft/Debian 签名,**Secure Boot 已支持**);
  UEFI arm64 仍用 ipxe-arm64.efi(未签名,Secure Boot 待后续);
- Secure Boot 签名信任:RHEL 系(rocky9/rocky10/centos7)kernel 签名已被
  Debian shim 信任(真机 2288H 闭环实证);kylin/uniontechos 国产发行版
  待验证(签名证书可能不在 Debian shim 信任列表);
- ramdisk 探针(`probe=ramdisk boot=pxe`):alpine 引导树 + probe overlay
  以第二段 cpio 追加进 initramfs(kernel 支持串联 cpio 段,同 early
  microcode 机制);modloop 经 `modloop=<http-url>` 提供——该参数与拼接段
  在 qemu harness(scripts/pxe-dev)验证,真机已 succeeded;
- qemu 与 `netdev user` 的内建 DHCP 不可用于 proxyDHCP 验证(slirp 的
  DHCP 在 qemu 进程内,宿主收不到广播)——harness 用 Linux bridge +
  dnsmasq 作真 DHCP(只分地址,不配 dhcp-boot),见 scripts/pxe-dev/README。
