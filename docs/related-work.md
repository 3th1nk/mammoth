# 相关开源项目对照(related work)与借鉴清单

mammoth 的生态位:**不依赖任何平台的独立裸金属装机引擎,以发行版原生应答
文件方言(kickstart / autoinstall / preseed)表达安装意图**。主流开源项目
均未轻量占据此位:Ironic 绑定 OpenStack,Metal3 绑定 Kubernetes,
Tinkerbell 换用 agent 工作流哲学,Pixiecore 只做引导层。

| 项目 | 本质 | 硬依赖 | 安装逻辑 | 与 mammoth 的关系 |
|------|------|--------|----------|-------------------|
| Ironic | OpenStack BMS 组件 | OpenStack 全栈 | IPA agent + deploy steps | 同域,重平台 |
| Metal3/BMO | K8s BMH 控制器 | K8s + Ironic | IPA(同 Ironic) | 同域,K8s 生态 |
| Tinkerbell | 工作流引擎 | 多组件 | osie hook 镜像 | 同域,agent 哲学 |
| Pixiecore | PXE 引导器 | 无 | 无(引导层) | ✅ 已吸收(M7 netboot 服务) |

## 借鉴清单(按 roadmap 对应)

1. **M7 PXE(已落地)**:Pixiecore(Apache-2.0,Go)的 **proxyDHCP** 模式
   ——不抢占现有 DHCP,旁路应答 PXE 客户端;"boot from API" 动态引导与
   mammoth 的 task-token URL 同构。实现差异:引导项注册在 PG
   (`netboot_entries`,跨 facet 一致)而非进程内存;NBP 全走 HTTP 后仅
   iPXE 二段链交由 TFTP;引导项按任务生命周期显式注册/注销(宽限/孤儿
   清扫),而非 Pixiecore 的会话内 TTL。
2. **ramdisk 探针**:Tinkerbell osie(内存 OS)与 IPA 的 hardware
   collection——dmidecode/lsblk/ipmitool 输出的结构化 schema 可直接对照;
   osie 还能刷固件/配 RAID,是探针的激进版形态。
3. **擦盘合规**:Ironic cleaning steps(NIST 800-88)可作为 wipe 策略的
   未来选项(superblock 清除之外的合规等级)。
4. **API 错误模型**:Metal3 BareMetalHost 的 ErrorMessage 分类与
   operational status 映射,供任务错误码的产品化呈现参考。
5. **反面参考**:IPA agent 模式要求包源能被 agent 消费(镜像制式限制);
   mammoth 的原生应答文件方言让安装逻辑落在发行版最成熟的路径上——
   这是架构上坚持方言抽象(而非自造 agent)的依据。

## 相关结论

- 三方言 × 发行版的真机矩阵验证(rocky9/ubuntu22/debian12/uniontechos)
  完成后,mammoth 在"无平台依赖 + 原生方言"维度上具备独立生态位;
- 与上述项目共存而非竞争:业务层若已有 OpenStack/K8s,可经其 API 驱动
  mammoth(mammoth 保持 API-first,任何编排器都可调用)。

## BMC 驱动与异构服务器适配(Ironic 沉淀的借鉴方式)

Ironic 的厂商硬件管理沉淀在 Python 库生态(proliantutils=HPE iLO、
python-dracclient=Dell iDRAC、irmc-client=富士通……),**语言不通,无法直接
搬代码**;可借鉴的是它的**适配组织方式**:

1. **现代服务器已 Redfish 统一**:Dell iDRAC9+、HPE iLO5+、Supermicro X11+
   原生 Redfish——mammoth 的 Redfish 驱动天然覆盖主流机型;Ironic 的厂商
   Python 库主要服务老固件/非 Redfish 设备(mammoth 由 IPMI 驱动兜底)。
2. **quirk 沉淀点 = bmccompat 兼容矩阵**:逐厂商真机实录(华为模式)——
   Ironic 驱动代码里的厂商坑(缺失字段、必须走 OEM 端点的操作、固件版本
   分界)正是我们的 compat 条目来源;Ironic 的已知名单可当**测试清单**用。
3. **能力接口模型**:Ironic 的 `supported_*_interfaces` ↔ mammoth 的能力
   接口(VolumeCreator/PhysicalDrives 已有)。可新增两个标准 Redfish 能力:
   - **BiosSetter**(Bios Registry:属性表读/写,厂商无关);
   - **FirmwareInventory**(SoftwareInventory 只读,固件基线核对)。
4. **cleaning steps 模型**:Ironic 的 NIST 800-88 擦盘规范任务化——wipe
   策略(superblock/整盘)之外的未来合规等级选项。

## PXE 装机实现经验对照(2026-09-17,debian13 d-i netboot 真机 11 连环修后定向调研)

调研对象:Tinkerbell Smee(原 boots)、MAAS/curtin、Ironic deploy interfaces、
Cobbler distro_signatures。背景:debian13 PXE 真机闭环一路修了 11 个发行版
安装器假设摩擦(见 HANDOFF 2026-09-17 条目),本节沉淀竞品对同类问题的
答案与可执行借鉴。

### 1. 架构趋势:全行业绕开"安装器内联跑"(最重要的结论)

- **MAAS**:PXE 先引导临时系统(ephemeral,与目标同版本)→ **curtin** 只做
  镜像解包 + 存储配置,d-i/subiquity 不在部署路径上;
- **Ironic**:IPA 内存系统 → direct deploy(HTTP 拉镜像流式写盘)或
  iSCSI+dd;其 anaconda 接口是后补例外,且默认预期 OS tarball 而非活装
  RPM([deploy interfaces](https://docs.openstack.org/ironic/latest/admin/interfaces/deploy.html)、
  [anaconda interface](https://docs.openstack.org/ironic/2025.1/admin/anaconda-deploy-interface.html));
- **Tinkerbell**:HookOS 最小内存系统 → tink worker 以**容器步骤**执行
  workflow,每步都是平台自己的代码。

mammoth 的 11 连环(netcfg 晚到、apt-setup 验签、by-hash、update-grub
chroot 之死、UOS anaconda 崩溃)全部属于"方言安装器假设摩擦"这一个 bug
类。**中期方向:试点自有最小 agent initramfs**(分区 + 从池装内核/包),
六阶段流水线与声明式 spec 原样承载,preseed/kickstart 方言降级为兼容
模式——与 related-work 开头"坚持方言抽象"的旧结论形成修正:方言抽象
的价值在兼容存量大,不应阻碍新增一条自持安装路径。

### 2. Smee/boots(Apache-2.0,Go,可借鉴代码)

boots 于 2025-12 归档,功能并入 [tinkerbell/tinkerbell](https://github.com/tinkerbell/tinkerbell)
单仓(网络引导服务现名 **Smee**)。与 mammoth 架构最像(DHCP/proxyDHCP +
TFTP + HTTP 脚本,硬件记录按 MAC 匹配):

- **proxyDHCP 三模式**:reservation / proxy / **auto-proxy**——auto-proxy
  对未登记 MAC 发放兜底引导脚本,mammoth 零注册入门(pending_machines)
  可借鉴此"未登记也给出路"的形态;
- **内置 syslog sink**:安装期收集带内 syslog——d-i 支持 `syslog=<host>`
  内核参数,安装器日志是 ramfs、重启即逝(2026-09-17 update-grub 尸检
  差点失据)。mammoth netboot 栈加 514/udp sink + 渲染时带参数,低成本
  高回报。**✅ 已落地(2026-09-17)**:netboot 服务内置 514/udp sink
  (`MAMMOTH_PXE_SYSLOG_PORT`),租约反查命中已武装任务的行带 `task_id`
  落 task_logs,debian netboot 内核参数自动带 `syslog=<机器面 host>`;
  端口被占仅告警降级(诊断非命脉)。
- **IP 认证的脚本 URL**(`/MAC/auto.ipxe`)vs mammoth 的 token 化池 URL:
  token 会进日志/代理,IP 认证形态值得斟酌。

### 3. curtin/MAAS(AGPL,只读设计)——离线源与签名的老战场

- 离线环境推荐**内联 raw PGP 块**传钥匙([curtin apt_source](https://curtin.readthedocs.io/en/stable/topics/apt_source.html));
  mammoth 的 key deb + stanza 等效;
- curtin 的源写在 `/etc/apt/sources.list.d/<文件>` 且支持 **per-file
  `signed-by`**——比 mammoth 现行的全局 `Acquire::AllowInsecureRepositories`
  更干净:post-install 写一个带 signed-by 的自持源文件、撤掉全局容忍,
  target 的 apt 回到正确签名姿态。**✅ 已落地(2026-09-17)**:post-install
  把 apt-setup 池行原地改写为显式 signed-by(容忍无条件 rm;钥匙保留
  trusted.gpg.d;deb822 形态跳过改写);
- curtin 的 apt 配置跑在"镜像解包之后"的 target(完整 /proc)——这解释了
  MAAS 为何不会踩到 mammoth 在 d-i chroot 里 update-grub 暴毙的问题,
  印证"装完再配置"的时序优势。

### 4. Cobbler(GPL,只读设计)——发行版元数据的数据文件化

`distro_signatures.json`:每发行版一条声明(kernel/initrd 相对路径、
bootloader 文件名、内核参数、架构、版本范围),社区维护、**加新发行版
不改代码**。与 mammoth OSDriver 注册制同构;可吸收为声明式数据文件,
把"接入新发行版"从写 Go 变成写 JSON。
**评估后不吸收(2026-09-18)**:曾落地全量 distros.json,评估后 revert
——Cobbler 的签名数据被一个通用引导引擎消费(路径/参数是纯事实),
而 mammoth 的 per-distro 参数是家族模板的语义开关,与 Go 实现强耦合;
发行版接入成本在模板与真机验证,数据化注册面是假性降本。决策记录见
docs/06 §6。

### 5. 对照结论:mammoth 的差异化与行动清单

领先项(竞品无对应物):真 shim→grubnet Secure Boot 链(竞品默认 iPXE
无 SB)、声明式 Install Spec(network v2/storage 三档)、六阶段证据化
流水线、零注册入门(pending_machines→claim)。

行动清单:①~~近期:netboot syslog sink + `syslog=` 内核参数~~
**✅ 已落地(2026-09-17,见 §2)**;②~~近期:
post-install 改写 sources.list.d + signed-by,撤全局 insecure 容忍~~
**✅ 已落地(2026-09-17)**:post-install 原地改写 apt-setup 池行为显式
`signed-by=`(钥匙保留 trusted.gpg.d,容忍文件无条件 rm;deb822 形态跳过),
ubuntu late-commands 同步补容忍清除(双方言对称);
③~~中期:agent initramfs 安装路径试点~~ **✅ 已落地并 qemu 双固件验证
(2026-09-18,docs/12-agent-initramfs.md)**——§1 的修正结论兑现:无方言
路径直接消费声明式 spec,新发行版接入面收敛到"池可达 + 声明";后续新
发行版优先评估 agent 路径,方言保留为兼容模式;
④数据化 distro 签名(OSDriver → JSON)——已入 roadmap"下一阶段"
第 3 项(跟随①定形态),与③联动评估。

## 6. Windows 装机机制对照(2026-09-19,windows2019 v1 落地前定向调研)

竞品分三条路线,没有第四条:

### 路线 A:镜像整盘投放(image-based)——Ironic/Metal3、MAAS

- **Ironic/Metal3**:IPA(Linux ramdisk)把 **预制的 Windows 整盘镜像**按 bitstream
  写盘(direct deploy),不做分区不做安装器;镜像由
  [cloudbase/windows-imaging-tools](https://github.com/cloudbase/windows-imaging-tools)
  在 Windows 构建机上产出(Autounattend.xml 挂虚拟软盘装机 + sysprep + cloudbase-init
  烘焙);首启配置交给 **cloudbase-init** 读 config-drive/metadata
  ([教程](https://cloudbase.it/create-windows-openstack-images))。Glance 镜像带
  os_type=windows + hw_firmware_type 属性。
- **MAAS**:[packer-maas/windows](https://github.com/canonical/packer-maas/blob/main/windows/README.md)
  产出定制整盘镜像(cloudbase-init 内烘),MAAS 3.x 解锁 Windows 部署需
  **license/OID**——Windows 在 MAAS 是许可墙后的二等公民。
- 本质:**金镜像工厂**模式。unattend 只活在镜像构建期;每次 Windows 版本/驱动集
  变更都要重造镜像;镜像工厂本身需要 Windows 构建机 + ADK。

### 路线 B:PXE → wimboot → WinPE——Foreman/Cobbler/Tinkerbell/FOG

- 经典配方([Foreman](https://community.theforeman.org/t/windows-provisioning-on-bare-metal/11609)、
  [Cobbler 文档](https://cobbler.readthedocs.io/en/latest/user-guide/windows.html)):
  iPXE 链 **wimboot** → HTTP 拉 bootmgfw.efi/BCD/boot.wim → WinPE 起来后经
  winpeshl.ini/startnet.cmd 挂 unattend → setup 源指向 install.wim 或网络共享。
  unattend.xml 是模板引擎渲染(Foreman WAIK 模板)。
- 代价:**BCD 精心构造** + boot.wim 驱动注入(DISM,Windows 工具链)——竞品为此普遍
  保留一台 Windows 构建机;Tinkerbell 用 HookOS(Linux)落地,Windows 仍走
  wimboot 链或镜像投放。

### 路线 C:WDS(Windows 原生)——所有第三方都绕开

依赖 AD/WDS 基础设施,跨引擎不可嵌入;且微软持续收紧 WDS 上 unattend 的玩法。

### 对照结论:mammoth v1 的位置与 v1.x 行动清单

1. **v1(虚拟介质 + 媒体根 autounattend)在竞品矩阵里是独一档**:零金镜像工厂、
   零 Windows 构建机、零 BCD/wimboot 管道——用 BMC 挂载官方原盘 + Setup 原生
   应答发现,把声明式 spec 翻译成 unattend,SetupComplete.cmd(wimlib 注入)承载
   回调/网络。代价是接受 iBMC 虚拟介质传输与官方介质安装时长(与 UOS DVD 同量级,
   真机已证明可行)。
2. **镜像路线我们刻意不跟**:金镜像工厂与 mammoth"声明式意图 + 官方介质"的立约
   方式相悖,维护成本(每版本×每驱动集×每语言)不适合个人/小团队维护的引擎;
   若未来有大客户诉求,可在 v2 层以"整盘镜像投放"作为一个 install 动作形态接入
   (agent 通路天然能 dd),不必改契约骨架。
3. **v1.x 的 Windows PXE 直接抄路线 B 的 wimboot 配方**:mammoth 已有 iPXE 二段链
   + netboot/files HTTP 树,wimboot 三件套(bootmgfw.efi/BCD/boot.wim)是纯文件
   服务,`windowsLayout` 家族扩展即可承载;BCD 的 MAC/架构差异化是主要工作量。
   boot.wim 驱动注入是 Linux 侧做不到的(DISM 是 Windows 工具链,wimlib 只能加
   文件不能注册驱动)——所以 **WinPE 识别不了盘就退回虚拟介质路线**,这决定了
   v1 虚拟介质先行的正确性;v1.x PXE 的前提是 inbox 驱动足够(LSI 3508 在
   2019 inbox,预期成立)。
4. **cloudbase-init 我们刻意不用**:竞品靠它 + metadata 服务做首启配置,而 mammoth
   的哲学是 SetupComplete.cmd 自包含(spec 烘焙,无 metadata 依赖,与 Linux 方言
   late-commands 同型)。代价是首启配置的表达力弱于 cloudbase-init userdata——
   当前 spec 面本来就窄,成立。
