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
| 统信服务器 V20(UOS) | `uniontechos` | **anaconda 定制**(RHEL 系安装树:AppStream/BaseOS/isolinux,非 d-i) | kickstart(同 `rocky9` 方言) | **full**(真机复核 2026-09-19) | MAC → 接口名在 %pre 安装期解析 | **full(真机闭环 2026-09-19:虚拟介质 + PXE 双通路零人工)**;⚠️ **方言约束:仅图形前端可用**——text 模式(text 指令/inst.text)下 UOS anaconda 自动分区建出 FAT16 而非 swap 且 Finish 组崩溃,驱动已强制 graphical(见下方根因节) |
| **Windows Server 2019** | `windows2019` | Windows Setup(bootmgr,`/sources/install.wim`) | **unattend**(autounattend.xml 媒体根自动发现,零内核参数) | **none**(wimboot 载体链已验,install 源 SMB 落地后翻 full) | MAC → SetupComplete.cmd 按 Get-NetAdapter MAC 绑定(装后 SYSTEM 首登录前落地) | 🔧 **v1 代码面就绪;真机窗口定案(2026-09-20):装机被 iBMC 6.41 固件缺陷挡在引导层**(虚拟 CD 无法 UEFI 引导 windows 介质,详见下方 windows 节);**wimboot-over-PXE 载体链 qemu 实证(2026-09-20,见下方落地小节):iPXE→wimboot→WinPE 全通,唯一悬段=install 源(SMB)**;终局 = agent apply-image |

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

### uniontechos 根因已定位并修复,真机已闭环(2026-09-18 qemu 复现 → 2026-09-19 真机零人工闭环)

- **现象(历史)**:全自动 kickstart(安装树/包/分区/网络全部正确,489 包
  安装完成)下,UOS 定制 anaconda(33.16.4.15)在 Finish 阶段崩溃:
  `dasbus.error.DBusError: max() arg is an empty sequence`(task_proxy.Finish);
  bootloader/eula/user 注入等 kickstart 参数层试验均无法绕过——因为真正的
  变量根本不在 kickstart 内容里。
- **根因(2026-09-18 qemu 五轮对照 + stage2 源码定位,代码级实锤)**:
  UOS 定制 anaconda 的 bootloader 模块在 resume= boot-arg 任务里
  `max(swap_devices, key=...)` **无空序列保护**——守卫条件写作
  `if blivet.arch.is_x86() or blivet.arch.is_loongarch() and swap_devices:`,
  运算符优先级使空列表检查永不生效(≡ `if is_x86():`)。**x86 + 安装结果
  无 swap 分区 = Finish 任务组必崩**("max() arg is an empty sequence" 从
  Storage 模块经 dasbus 传回 UI 的 task_proxy.Finish)。五轮矩阵:
  | ks 形态 | 显示模式 | 安装结果 swap | 结论 |
  |---------|---------|---------------|------|
  | 手写 autopart | 图形(SATA/USB 两轮) | 0x82 4G | ✅ 全绿(装后自举) |
  | mammoth ks(显式分区) | text | 无 | ❌ 崩(与真机同栈) |
  | 手写 autopart + inst.text | text | **0x06 FAT16(非 swap!)** | ❌ 崩 |
  | mammoth ks(显式分区) | 图形 | 无 | ❌ 崩(证伪"仅 text") |
  - 载体无关:SATA / USB CDROM(iBMC 虚拟介质形态)结论一致;
  - 历史排除试验失效的原因:真机恒为 text 模式(mammoth 引导参数硬编码
    `inst.text`),全部试验都在"无 swap 崩溃"下跑,变量无效;
  - 附带发现:**text 模式下 UOS autopart 产出 FAT16 而非 swap**(图形模式
    同 ks 产出 0x82)——text 前端本身也不可靠;
  - 代码位置:stage2(rootfs.img)内
    `usr/lib64/python3.6/site-packages/pyanaconda/modules/storage/bootloader/base.py:737`;
  - 证据:scripts/uos-dev/(qemu 复现 rig),traceback 存档于运行目录
    anaconda-crash-traceback.txt。1050a(md5 da805754…8641,与真机同版)。
- **修复(kickstart 驱动 dialect 层,回归钉
  `TestRenderUniontechosRunsGraphical`)**:
  1. **无 swap 自动补齐**:spec 未声明 swap 时追加
     `part swap --fstype=swap --size=2048`(直击 max(swap_devices) 崩溃点;
     用户已声明 swap 则原样尊重);
  2. **uniontechos 固定 graphical、引导参数去 `inst.text`**:text 模式下
     UOS autopart 连 swap 都建错(上表第 4 行),graphical 是经验证的可靠
     路径;家族其余成员保持 text(rocky9/kylin text 真机跑通,行为不变)。
- **真机闭环(2026-09-19,2288H V5 / iBMC 6.41,虚拟介质)**:六阶段全绿
  **零人工干预**——无人值守首启 + SSH 凭据直通(hostname uos-2288h)+
  swap sda4 2G 激活 + 分区与 spec 一致(EFI 512M/boot 1G// 3.6T)。**真机
  首验暴露并修复了两层 qemu 未覆盖的缺陷**:
  1. 装机网络:机房无站点 DHCP + 双口 LOM,`ip=dhcp` 的 dracut DHCP 落在
     未插线的 eno2 上,ks 永远拉不到——spec 声明静态网络(MAC 钉 eno1 +
     池段地址)即解(三层寻址策略既有语义,装机期与装后一致);
  2. swap 预算(首次修复的真机回归):`part swap` 追加在主 ks 体(无
     --ondisk、不计 growSizeMB),显式容量场景总请求超盘 ~1.5G,anaconda
     "Unable to allocate requested partition scheme" 落回交互 hub(qemu
     测试盘无容量走 --grow 兜底,未暴露)——swap 注入 boot 盘分区列表
     头部、随行生成 --ondisk/$Dn 且计入 grow 预算即解(f951f98);
  - 附带:介质目录迁 /data(18G 的 / 放不下 8.2G ISO 拷贝;nfs:// 源
    EnsureISO 会落本地);水位闸显示错误顺修;1050u2a 的 33.19 anaconda
    在 qemu TCG 下 Storage 模块 600s 启动超时,qemu 不可筛。
- **恢复路径**:~~真机复验~~ **✅ 已闭环(虚拟介质 + PXE 双通路)**;
  复验口径改定(2026-09-19):1050u2a 同代增量验证价值低,目标改为
  **UOS 当前最新发行版 ISO**(等官方渠道到位,双通路各一轮 API 提交)。
- **⚠️ 长期方言约束(已由驱动强制,人工排查/手写 ks 时必须遵守)**:
  uniontechos **只能用图形前端**(`graphical` 指令、内核参数**不得带
  `inst.text`**)。text 模式两处已证实的坑:①Finish 任务组
  `max(swap_devices)` 空序列崩溃(UOS 定制 bootloader 模块的运算符
  优先级笔误,与 swap 是否存在无关地恒崩);②autopart 在 text 前端下
  建出 0x06 FAT16 而非 swap(五轮矩阵第 4 行)。qemu 复现见
  scripts/uos-dev/;若未来有人提议"headless 场景换 text 模式",先读
  本节与该矩阵——UOS 的 text 前端在 33.16 定制版上是坏的。

### 新增发行版

实现 `render.OSDriver`(六个方法)+ 注册一行,编排层零改动:
`Registry.For(distro)` 路由、`KeepPartitionSupport()` 进入提交门禁与矩阵导出。

### PXE 网络引导(M7,boot.strategy=pxe)

| 发行版 | PXE 支持级 | 说明 |
|--------|-----------|------|
| rocky9 / centos7 / kylinv10 / kylinv11 | full | anaconda/dracut 内核对网络引导原生;`inst.repo=nfs:` 安装源复用 nfsx 导出,`inst.ks=` 走 HTTP,与 ISO 通路零差异 |
| uniontechos | full(**真机复核 2026-09-19**,2288H 零人工闭环,~6 min:NFS 线速 vs 虚拟光驱 35 min) | 同 kickstart 家族机制(shim 链 + per-MAC cfg + `inst.repo=nfs:`);装机期静态网内核参数(ifname 钉 MAC)与虚拟介质轮共用 |
| rocky10(UEFI-only 媒体) | full | 引导文件同为 `images/pxeboot/*`,UEFI 侧无虞;BIOS 引导上游已移除(镜像 UEFI-only),不做 BIOS PXE。驱动声明 `FirmwareUEFIOnly`,派给观测为 BIOS 固件的机器在提交期即被 `SCHEMA_FIRMWARE_MISMATCH` 拒绝(不阻塞同批其他机器) |
| ubuntu22/24 | full(**真机闭环 2026-09-17**,2288H:22.04 零人工重装 + 24.04 重装+重启自举+SSH 钥匙直通) | casper kernel/initrd 从 ISO 提取;**NFS squashfs 源**——ISO 解包至共享池树,casper `netboot=nfs nfsroot=` 挂载 live root,不做整 ISO 进内存(4GB 级 tmpfs 写满教训);nfsopts 强制 `tcp,v3`(klibc nfsmount 默认 UDP,现代 nfsd 无 v3/UDP);`BOOTIF=01-<mac>` 按 MAC 钉设备(udev 改名致 ipconfig 打空);内核参数分号转义(GRUB 把 `;` 当命令分隔符,`ds=nocloud-net;s=` 之后参数全丢);spec 静态网络翻译为 ip= 内核参数(site-DHCP 共存环境 mammoth 不做地址权威);**qemu 实测注记**:casper NFS 语法为 `boot=nfs nfsroot=host:/path`(冒号必需);macOS nfsd(UDP-only)不可作验证宿主,真机 Linux nfsd 无此限制 |
| debian12/13 | full(**真机闭环 2026-09-17**,debian13 trixie,2288H:零人工六阶段全绿、重启无人值守自举) | **载体 = d-i 官方 netboot.tar.gz**(`MAMMOTH_PXE_DI_NETBOOT`;ISO 自带 initrd 为 cdrom flavour,网络上不可用);**安装源 = ISO 解包为签名 HTTP 池**(mammoth 池钥匙重签 Release,armor detached;钥匙经 mammoth-key deb 随 debootstrap 进 target——trixie 无 apt-udeb,apt-setup 的 verify 在 chroot /target 跑),preseed mirror 指向池——离线语义保持,不上游镜像;装后收尾:池行显式 signed-by、容忍配置清除、update-grub 兜底;**实测注记**:netinst 池裁剪 netboot 专用 udeb(kernel-modules-di 等),纯 ISO 池需 archive 补齐(fill-udebs 机制);netcfg 走内核参数(preseed/url 在 netcfg 后加载);by-hash 货栈需回填(ISO 声明 Acquire-By-Hash 却不带货栈) |

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

## Windows(v1:虚拟介质 + UEFI-only)

> 2026-09-19 启动。选型依据:Windows 官方单一完整 ISO(Standard/Datacenter ×
> Core/Desktop 同盘,装时按 /IMAGE/NAME 元数据选 SKU),无 Linux 式轻量 netboot;
> 推荐裸金属默认 **Standard Core**。2019 与 2022 镜像均已在 248
> (`/data/os_iso/{windows2019,windows2022}/`,2022 于 2026-09-20 到位);
> 当前仅注册 `windows2019`,2022 为构造变体待注册验证(媒体布局同族)。

**形态与机制**:

- **驱动** `internal/render/windows`,`windows2019`(2022 后续为构造变体);产物两件:
  `autounattend.xml`(媒体根,Setup 原生发现,KernelArgs 为空)+ `mammoth/SetupComplete.cmd`。
- **Builder**:新布局家族 `windows`(识别 `/sources/install.wim`)。`rebuildPatchedISO`
  的 El Torito 重放自动保真 etfsboot/efisys 引导记录;补充 `-udf -iso-level 3`
  (install.wim >4GiB,UDF 是承重墙);`patchBootConfigs` 空操作(bootmgr 无参数可打)。
- **完成回调**:SetupComplete.cmd 经 **wimlib** 注入 install.wim 全部镜像索引
  (`/Windows/Setup/Scripts/SetupComplete.cmd`)——Windows 的 late-commands 对应物:
  SYSTEM 身份、首登录前、网络栈已就绪。内容=完成回调 POST(PowerShell
  Invoke-WebRequest)+ 声明式静态网络(按 Get-NetAdapter MAC 绑定 New-NetIPAddress/
  Set-DnsClientServerAddress;安装期无网络契约,装后落网即 ubuntu late-command netplan
  的同型)。verify 阶段零改动:未配 SSH 凭证时完成回调即验证面(既有语义)。
- **分区**(UEFI GPT):ESP(声明则用其大小,未声明自动补 300MiB)+ MSR 16MiB(mammoth
  自动插,spec 不可见)+ spec 其余分区(ntfs/fat32;Grow 仅限末分区→Extend);
  InstallTo=OS 分位。BIOS/MBR 布局 v1 不渲染。

**v1 边界(显式拒绝,不静默重释)**:PXE none(WinPE 链挂 v1.x)、keep none、
RAID 拒绝、bond/vlan 拒绝、用户脚本拒绝、非默认路由拒绝、FirmwareSupport=uefi_only
(渲染面是 ESP+MSR 形态,BIOS 机提交即拒)。SKU 固定 SERVERSTANDARDCORE。

**qemu 已验证(2026-09-19/20,TCG)**:OVMF→bootmgfw→WinPE 引导链、autounattend
被完整受理(WillShowUI=OnError 下零交互 UI)。**真机教训(2026-09-20)**:Server 2019
的 Setup 要求 UserData 内 **<ProductKey> 元素存在**(空 Key + /IMAGE/NAME 选 SKU),
缺失即弹 "无法从无人参与应答文件读取 <ProductKey> 设置" 中止——已修复并 golden 钉住。
TCG 全装挂机因固件交互窗口(UEFI Shell→bootx64→press-any-key→BCD 菜单)的定时
盲发不可靠,收口于应答受理层;装机闭环与 SetupComplete 回调验证并入 2288H 真机窗口。
**待真机** → **真机定案(2026-09-20)**:2288H(iBMC 6.41 + BIOS 8.20)的
虚拟介质 UEFI 引导对 windows 介质**固件级失败**——原版 2019/重打包 2019/
原版 2022、NFS 服务端挂载/KVM 客户端重定向、Once/Continuous 全灭
("EFI USB Device (Virtual DVD-ROM VM 1.1.0) boot failed."),而同通路
alpine 270M ×4 与 UOS 8.2G 均正常引导(大小无辜)。装机代码面全部就绪
且经 qemu 应答受理层验证,瓶颈仅在固件;全装闭环与 SetupComplete 回调
验证**等 workaround**(物理 USB / iBMC 升级复验 / builder 增 Ventoy 式
grub 链式引导,详见 compat/huawei.md windows 虚拟介质节)。
LSI SAS3508 inbox 驱动验证随闭环一并推迟(预期不变:2019 有 inbox
MegaRAID 驱动;iBMC 6.41 虚拟介质挂 5GB ISO 已实证可行,2022 5.5G 同)。

**通路路线决策(2026-09-20,定案)**:对照行业三派——①WinPE 走网络
(WDS/MAAS 式,或 iPXE wimboot 从 HTTP 喂 bootmgfw/BCD/boot.sdi/boot.wim
四件套);②Linux 侧 apply-image(wimlib apply + hivex 预烤 BCD +
unattend 落 Panther,MAAS-adjacent/Tinkerbell-adjacent 学派);③介质侧
修复(Ventoy 式 grub 链载,prosumer 世界实证充分但非 provisioning 主
航道)——定策:**v1.x 主线 = windows PXE 载体直接落 wimboot-over-HTTP**
(4 件套 builder 全可提取;网络引导绕开 El Torito,顺带根治 iBMC 6.41
这类固件缺陷,外部 DHCP+TFTP 逃生门即其投递载体;wimboot 为 ipxe 项目
GPL2 组件,按 shim/grub 的 fetch-and-pin 先例纳管);**终局 = agent
apply-image**(复用已实证的 agent 引导与声明式落盘,SetupComplete/回调
面零改动,不背各家 BMC 的 CD 怪癖);**Ventoy 式降级为可选介质侧实验**
(一次重打包即可验证本固件认不认 grub 链载,认了算白捡缓解)。原版
windows 介质的隐匿 El Torito(Ldsiz=1)在此固件必死,重打包规范化条目
同死,故介质侧任何方案以"先引导成功"为验收,不预设。

### wimboot-over-PXE 落地(2026-09-20,v1.x 主线兑现)

**载体形态(`internal/render/windows` + `internal/builder/wimboot.go`)**:
wimboot 载体的**投递链已 qemu 实证闭环**(iPXE → wimboot → bootmgfw → bootmgr
→ WinPE 桌面),但 **PXESupport 保持 none**——install 源未落地(见下)。
与 Linux 方言的"内核+initrd+独立安装源"三件套不同,Windows 载体的 per-task
部分只有**小文件**:builder 从 prepared tree(与虚拟介质共享的 ISO-sha 缓存)
取 bootmgr/bootx64.efi/BCD/boot.sdi/原版 boot.wim,经 wimlib 向 boot.wim 的
Setup 镜像(Boot Index)灌入 `autounattend.xml`(根)与 `mammoth/task.json`
(KB 级)。投递面是 iPXE(wimboot 唯一官方宿主,`kernel wimboot` + 逐文件
`initrd` 带内存名):per-task 树投 `wimboot` + 五个引导文件,iPXE 脚本按 entry
的 wimboot 形态渲染(`internal/netboot/script.go`);grub 侧对 wimboot entry
渲染显式 exit(shim→grubnet 链无 wimboot 路径,干净掉盘)。

**投递路由(builtin + external 同源)**:wimboot 是 bzImage 形态的无签名二进制,
shim→grubnet 链无法把文件递给 chainload 的镜像(grub UEFI 加载器无 wimboot 路径,
wimboot 文档也只认 iPXE)——因此 **wimboot entry 的 UEFI x64 客户端由 proxyDHCP
按 MAC 解析后直发未签名 `ipxe-amd64.efi`**(普通 PXE ROM 的 BIOS 侧本就经
undionly 进 iPXE,零改动);external 逃生门的 kit 增导出 `ipxe-amd64.efi`,
dnsmasq example 附按 MAC 钉 Windows 机器的示例段。

**install 源:唯一悬而未决的一段(qemu 实证,2026-09-20)**:
`install.wim` **不能灌进 boot.wim**——增强后 wim 总量 4.8G 跨 4 GiB 边界,
Server 2019 的 bootmgr ramdisk 路径直接拒载(`0xc0000225 \windows\system32\
boot\winload.efi ... missing or contains errors`);同链路对照:原版 boot.wim
与仅灌小文件的 boot.wim 均 WinPE 正常启动、灌入的 autounattend 被 setup
读取解析(DiskConfiguration 分析错误框为证,失败本身是 guest NVMe 枚举/
安装源缺失的次生现象)。结论:**安装源走网络(SMB 共享——WDS 同款形态,
go-smb2 只读共享 + setup 自 InstallFrom 指引),这是 PXESupport 翻 full 的
唯一前置**。此前评估过的"install.wim 并入 boot.wim = X: 即虚拟 DVD"形态
就此否定。

**边界(显式)**:
- **Secure Boot 必须关闭**:固件只验 NBP 层,wimboot/iPXE 均无微软链签名;
  站点自行 MOK 纳管 iPXE 属部署层策略,mammoth 不拥有。
- **目标机内存 ≥8G**(WinPE 466M wim + SMB 装机运行时,余量充足;此前
  4.8G wim 形态的内存压力随否定一并消失)。

**qemu 实验记录(2026-09-20,external rig `scripts/windows-dev/
external-win-e2e.sh`)**:①SB OFF 轮:站点 DHCP(dnsmasq 按 MAC 钉
ipxe-amd64.efi)→ iPXE re-DHCP(opt 175 tag)→ boot.ipxe 蹦床 → per-MAC
wimboot 脚本 → HTTP 拉 wimboot+五件 → **WinPE 完整启动**(原版与小文件
增强版两轮,后者 setup 读到 autounattend);②>4G boot.wim 轮:0xc0000225
(winload.efi),否定"install.wim 并入"形态;③SB ON 轮未跑——SB 下固件
拒未签名 NBP 是架构性边界(与 arm64 AAVMF 无 PXE 栈同类),qemu 补测
优先级低。**环境坑(本机)**:brew wimlib 在 macOS 迁移/升级后签名失效,
进程启动即 UNE 挂死(连 `--version` 都挂),`codesign --force --sign -`
对 bin/lib 重签即愈;Docker Desktop 默认 VM 内存 7.65G 装不下 8G guest
(settings-store.json `MemoryMiB=16384` 后重跑通过)。
