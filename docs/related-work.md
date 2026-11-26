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

> **2026-11-07 现状注记**:Foreman 3.17 季度节奏活跃(继续对照);Cobbler
> 维护缓慢(仅参考);**xCAT 已 EOL 归档(2023-12),移出竞品清单**;
> Tinkerbell 进 CNCF Sandbox 且 tink/boots/smee 已并入单仓 monorepo。
> **2026-11-17 增量**:RackShift(国内唯一开源裸金属平台)2023-12 起停更
> 确认,国内服务端开源位空缺(§8.4);完整扫描见 §7、§8。

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
  (`MAMMOTH_PXE_SYSLOG_PORT`),租约反查命中已登记装机任务的行带 `task_id`
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
   (agent 通路天然能 dd),不必改契约骨架。——**该预留点已立项**(2026-11-06,
   roadmap「镜像直写通路」设计定稿:工控机 POC 诉求落地 + 业界 direct deploy
   主线对齐,dd 运行时 = agent initramfs 第三 payload)。
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

## 7. 竞品扫描(2026-11-07:开源引擎 2025-26 现状 + 商业功能面)

> 2026-11-07 定向调研,产出 roadmap「下一阶段」第 11~19 项;本节沉淀
> 结论与证据,行动项一律看 roadmap,此处不重复展开。

### 7.1 开源引擎 2025-2026 现状

| 项目 | 最新 | 2025-26 关键动向 | 对 mammoth 的意义 |
|------|------|-----------------|------------------|
| MAAS | 3.7.x(2025-12) | BlueField-3 DPU 装机;**maas-agent EOL 回退 classic 架构**;Terraform provider 活跃;FDE 无原生支持 | 常驻 agent 路线被其自身放弃佐证:mammoth 的 agent 是 initramfs 载荷(用完即走)非常驻,不受此劫;Terraform provider 是 API-first 标配生态件(roadmap 14) |
| Ironic | 2026.1 Gazpacho | 新增独立 `ironic-networking`(脱离 Neutron 直管交换机);VirtualMedia NFS/CIFS/SMB 自动探测;**servicing(装后免重装动作)成一等状态机路径**;固件更新走 UpdateService | servicing 风向支持动作面扩展思路;固件写路径(roadmap 15)是其标准范式 |
| Metal3 | BMO v0.14.x | Redfish 快速检查模式;**Live Updates/Servicing**;HostFirmwareComponents/Settings CRD;HostClaim/HostDeployPolicy 新抽象 | HostClaim 与 mammoth 零注册 claim 同型,方向被验证;BIOS 基线巡检(roadmap 13)是其 HostFirmwareSettings 企业形态 |
| Tinkerbell | CNCF Sandbox | tink/boots/smee 全部归档并入 monorepo;HookOS v0.22 + TUI 诊断面板提案 | 单二进制/组件做减法与 mammoth 路线一致;HookOS TUI 是 agent 调试体验参照 |
| Sidero/Omni | Infra Providers | PXE→agent 模式自动注册;staged networking | 零注册入门的业界同型(方向验证) |
| Foreman | 3.17(2025-12) | 季度节奏活跃 | 继续对照(模板与 lifecycle) |
| Cobbler | 3.3.x 维护 | 3.4.0 延期后发布,社区小 | 降级为仅参考 |
| xCAT | **EOL 归档** | 维护者 2023-12 宣布 EOL,最终 2.16.5 | **移出竞品清单** |

### 7.2 三个功能演进风向

1. **装后 servicing 成为一等能力**——Ironic servicing 与 Metal3 Live Updates
   汇合成同一定义:装机引擎不结束于"装完",需对已装机器执行固件/配置/
   维护动作而免重装。mammoth 对应面:动作框架(erase_drives/bios 先例)
   + rescue mode(roadmap 11)+ 生命周期收尾(roadmap 18)。
2. **OS 交付镜像化(bootc)**——RHEL 10 Anaconda 新增 `bootc` kickstart
   指令:安装器直接从 OCI registry 拉容器镜像完成安装,bootloader 由容器
   内 bootc 自写;Ubuntu subiquity/Debian 未跟进,MAAS 未跟进。OS 交付物
   从"ISO+应答文件"变为"OCI 镜像+最小安装器",安装器退化成"分区+引导+
   拉镜像"——与 mammoth agent 通路/dd 直写天然同构(roadmap 16)。
3. **固件即一等资源**——Redfish UpdateService 写路径已成 Ironic/Metal3
   标准范式;mammoth 只读 FirmwareInventory 之上的写路径是自然延伸
   (roadmap 15),两段式确认契约沿 BIOS/ERASE 先例。

### 7.3 商业功能面(差异化清单)

- **Equinix Metal 2026-06-30 正式关停**,官方引导自托管迁移(Tinkerbell
  同源)——独立装机引擎的市场叙事获外部验证;
- **rescue mode 是海外裸金属云标配**(Latitude.sh/phoenixNAP:内存 RAM OS
  + SSH 修复),自研引擎普遍缺失——mammoth 全件已有,立项 roadmap 11;
- **国内云厂商装机面**:华为云 BMS 重装 API(仅重装系统盘、数据盘不动)+
  私有镜像共享/导出 + 一键重置密码;腾讯黑石重装可自定义 RAID 级别;
  阿里云 EBM 批量主机名/自定义数据/实例诊断——均为云上形态,mammoth 的
  私有化面无对应义务,但"重装不动数据盘"的语义值得 spec 层留意;
- **RackN/Digital Rebar**:Day 0/1/2 全生命周期宣传,decommission 是
  重点环节——对应 roadmap 18;
- **信创**:麒麟 V10 服务器版与 openEuler 均为 anaconda/kickstart 系,
  与 mammoth kickstart 方言同族(openEuler 驱动接入已立项,见 roadmap
  第 9 项子条目),差的是逐发行版真机验证;
- **GPU/AI 机群**:K8s-on-bare-metal(Meta/CoreWeave)主流化,机器管理层
  产品化(vCluster vMetal 等),装机速度/节点 bring-up 自动化 SLA 化;
  MAAS 的 DPU 装机是同类风向——对应 P3 候选组(roadmap 19)。

### 7.4 行动清单

全部落 roadmap 11~19 项(P1:rescue mode → 装后验收探针 → BIOS 基线
巡检 → CLI/Terraform;P2:固件升级 → bootc → TPM-LUKS → 生命周期收尾;
P3:批量扇出/GPU 探针/容量轮/DPU,条件触发)。本节不另立行动项。

## 8. 竞品增量扫描(2026-11-17:特性面补扫 + GitHub 全量补扫)

> §7 之后的一轮增量调研,只列 §7 未覆盖的候选,分两半:①特性面补扫
> (Digital Rebar/NVIDIA BCM/SUSE Elemental/Talos Omni/NetBox 生态/
> 云厂商 BMS/数据中心 burn-in 实践/BMC 凭证轮换);②GitHub 装机相关
> 热门项目全量补扫(同域漏网引擎 + 邻域生态 + 淘汰名单)。
> 行动项落 roadmap 20~24,本节沉淀证据,不重复展开。

### 8.1 特性面增量(P1,纯既有能力组合)

| # | 候选特性 | 竞品原型 | mammoth 对应面 | 落地形态 |
|---|---|---|---|---|
| 1 | 硬件老化/验收测试包(burn-in) | MAAS testing scripts 的"可执行"半边;数据中心实践上架必做 4~24h 老化([Zenlayer QC:普通机 ≥4h、GPU 机 24h](https://docs.console.zenlayer.com/welcome/bare-metal/best-practice/bare-metal-cloud-server-quality-control)),GPU 集群验收普遍含 burn-in([1Legion](https://1legion.com/burn-in-and-acceptance)) | 第 12 项验收探针只做了"只读采集"半边;ramdisk 探针是载荷载体(rescue/agent 同型),spec.scripts 前后置已有 | 机器级动作 run_diagnostics:agent 内存态跑 stress-ng/memtest/fio/(GPU 机型)nvidia-smi 压测,结果入 acceptance_reports 同表——与验收探针拼成"验收=采集+老化" |
| 2 | BMC 凭证轮换动作 | DMTF 安全实践:per-BMC 唯一随机密码 + vault 驱动轮换,Redfish `ManagerAccount.Password` PATCH 是标准操作([Redfish 安全文](https://safeguard.sh/resources/blog/redfish-explained-dmtf-server-management-api-and-how-to-secure-your-bmc)、[华为 iBMC 官方同操作](https://support.huawei.com/enterprise/en/doc/EDOC1100266817/8d3a25ef/changing-the-bmc-administrator-account-and-password)) | credentials 只写永不回显已有;等保/信创合规是目标用户群刚需 | 动作 rotate_credential(新密码只写语义不变,两段式确认沿 BIOS/ERASE 先例);批量轮换走既有批量 job;注意改密后旧会话失效的任务时序 |
| 3 | 发现规则/自动 claim 策略 | Foreman Discovery Rules、Metal3 HostDeployPolicy、DRP classification——"零注册之后不该停"三者共同验证 | 零注册 pending_machines→claim 已落地,但 claim 是人工的 | claim 规则(按 MAC 前缀/机型/固件底座匹配→自动升格+赋标签/BIOS profile),纯策略层不动契约 |
| 4 | 硬件画像自动标签 | MAAS 按 commissioning 事实自动打 tag(XPath 匹配硬件)、DRP profile classification | batch-labels 已有,缺"按盘查事实规则自动打标" | 标签规则面(盘查 schema 简单谓词,如 gpu/nvme-raid/10gbe),是 GPU 机群分组扇出(roadmap 19)的天然前置 |

### 8.2 特性面增量(P2,条件触发)

- **AI/GPU 机群 bring-up 模板**——[NVIDIA Base Command Manager](https://www.nvidia.com/en-us/data-center/base-command-manager)
  是 AI 机群装机标杆(软件镜像蓝图+驱动/CUDA+节点健康门禁+Slurm/K8s);
  mammoth 不学平台面,组合既有件即可:GPU 探针(roadmap 19 已列)→ 增量 =
  GPU 健康面(ECC/温度)入盘查 → 8.1#1 老化包(GPU 压测)→ 驱动/CUDA 经
  package_source+scripts(已可达)→ 打包为可复用 bring-up 模板(8.1#4
  标签做分组)。装机速度 SLA 化是 AI 机群趋势,六阶段耗时统计已有数据基础。
- **带内硬 RAID(agent storcli payload)**——HookOS/IPA 均在内存系统做
  RAID(storcli/megacli),不依赖 BMC OEM;configure_raid 现走 BMC
  VolumeCreator(OEM 面窄,华为 OEM 回退已实录)。agent initramfs 第三
  payload 载体可承载 storcli:BMC 不支持 VolumeCreator 的机型 +
  **headless 机器也能配 RAID**(与 headless 形态协同),渲染即拒面可收窄。
- **库存导出目标(NetBox DCIM)**——[NetBox](https://netboxlabs.com/products/netbox)
  是私有化机房事实标准 DCIM/NSoT 但无装机能力,与 mammoth"工作缓存"
  定位天然互补:webhook 投递器已有,加 NetBox 适配器(机器/网卡/盘→
  NetBox device/component)。符合非目标边界:mammoth 仍不做权威资产
  管理,只做采集器角色。
- **自定义 iPXE 引导脚本逃生门**——Latitude.sh 等裸金属云的 custom
  iPXE script:netboot iPXE 树+per-MAC 注册已有,暴露"引导项覆盖"端点
  即可,token 生命周期沿任务先例。
- **重置密码动作**——华为云/阿里云 EBM 均有一键重置密码(§7.3 已记未
  立项):rescue flow 同型 one-shot agent payload(内存态 chroot 改密),
  复用载体/互斥门,不进安装流水线。

### 8.3 场景观察(只记录,不建议立项)

- **多站点管理面**(DRP Multi-Site Manager:站点自治+工作流全局同步)
  ——edge 代理只管到网段级;多机房统一面与单二进制边界冲突,等私有化
  多机房真实客户。
- **白盒交换机 ZTP**(ONIE/SONiC)——netboot 基建(proxyDHCP+TFTP+HTTP)
  完全可复用,但跨域;机架级交付场景提及即可。
- **无盘常驻节点**(Warewulf 谱系,HPC 内存盘节点)——与"落盘安装"
  哲学相反,观察(Warewulf 4 由 CIQ 维护,活跃,见 §8.4)。
- **TPM 远程度量认证**(Keylime 式)——roadmap 17(TPM2+LUKS sealed key)
  之上的延伸,观察。
- **SEL 带外健康 watch**(BMC SEL 轮询→事件→webhook)——注意与已排除
  的"常驻**带内**故障感知"的边界:带外 BMC 轮询不在排除范围,SEL 读
  已有,成本极低,可作健康面小补充。
- **Elemental/Omni 的不可变 OS 舰队管理**——OS 级范式(A/B 升级),
  mammoth 装完即交棒,不跟。

### 8.4 GitHub 全量补扫(装机相关热门项目,star 数 2026-11-17 实测 GitHub API)

**同域漏网引擎**(均未进本清单,按热度):

| 项目 | 热度/状态 | 说明与处置 |
|------|------|------|
| [Ventoy](https://github.com/ventoy/Ventoy) | 80.9k★ 活跃 | USB 多 ISO 启动,装机类全 GitHub 热度之王;个人装机 UX 方向标(免烧录、链式引导任意 ISO 与 builder 介质重打包同源),与机房批量范式相反,记录 |
| [netboot.xyz](https://github.com/netbootxyz/netboot.xyz) | 12.6k★ 活跃 | iPXE 菜单聚合全发行版安装器;"菜单即入口"体验可对照零注册 pending 机器 console 面。定向深化对照见 §9 |
| [Rescuezilla](https://github.com/rescuezilla/rescuezilla) | 2.8k★ 活跃 | Clonezilla GUI 化;rescue 场景消费级 UX 参照(boot_rescue 已落地) |
| [FOG](https://github.com/FOGProject/fogproject) | 1.7k★ 活跃 | PC 克隆/镜像管理;已排除领域(范围 B),活跃度佐证排除决策 |
| [Matchbox](https://github.com/poseidon/matchbox) | 1.4k★ 活跃 | Flatcar/CoreOS 声明式 PXE profile+ignition;与 netboot_entries 同型,对照 |
| [Warewulf 4](https://github.com/warewulf/warewulf) | 673★ 活跃(CIQ) | HPC 无盘容器 OS 供给;§8.3 无盘场景的真实标的 |
| [RackShift](https://github.com/fit2cloud/rackshift) | 643★ **2023-12 停更** | 国内唯一开源裸金属平台事实弃维护——**国内开源位空缺**;功能面可当中文市场用户预期清单 |
| [FAI](https://github.com/faiproject/fai) | 415★ 活跃 | 德系老牌无人值守安装(NFS+class 体系);class 分组组织方式可参考 |
| [Stacki](https://github.com/Teradata/stacki) | 237★ 2023 停更 | Teradata 装机引擎,半弃维护 |
| [ezio](https://github.com/tjjh89017/ezio) | 228★ 活跃 | **BitTorrent 分发装机**;大规模 dd 批量装机的带宽优化思路,挂接 roadmap 19 容量轮 |
| [Grendel](https://github.com/ubccr/grendel) | 72★ 活跃 | HPC 装机单二进制 Go(DHCP/DNS/PXE 一体);哲学同款,小众 |

**淘汰名单**(佐证赛道淘汰率,移出扫描):Razor(Puppet,2020 归档)、
Fuel(Mirantis,2019)、Mayu(Giant Swarm,2023)、tink 旧仓(并入
monorepo)、xCAT(2023-12 EOL,已移出)。

**邻域生态**(装机周边,按场景记名):**Beaker**(Red Hat 硬件实验室
调度)+LAVA(Linaro)+**Labgrid**(552★)——**硬件 CI 场景**(申请机器→
装机→跑测试→还机),信创适配实验室可能即此用户群,API-first 引擎可作
其执行层(本轮新发现场景);Packer+packer-maas/osbuild/mkosi/
ignition-butane——金镜像路线工厂引擎(已排除路线,bootc 重开时的
工具链);Mender(1.2k★)/Kairos(1.9k★)/balena/RAUC——边缘舰队 OTA
(工控 Day-2,交付边界外,客户可能问);OSDCloud(254★)——Windows
社区 WinPE 云镜像装机(§6 三路线之外的第四实践,驱动包管理可参考);
ShredOS/nwipe——擦盘启动盘行业惯例(decommission 已落地,生态位
记录);OpenBMC(2.6k★)——compat 三底座之一,非竞品;
[awesome-baremetal](https://github.com/alexellis/awesome-baremetal)——
新项目发现源,收藏。

### 8.5 行动清单

拟落 roadmap 20~24(P1:burn-in 测试包 → BMC 凭证轮换 → 发现规则 →
硬件画像自动标签;P2 候选组:AI bring-up 模板/带内 RAID/NetBox 导出/
自定义 iPXE/重置密码,条件触发)。本节不另立行动项。

**结论三条**:①同域无新威胁,有价值的反倒是叙事证据(RackShift 停更
+Stacki 衰落+淘汰名单→独立引擎生态位成立);②中文服务端装机开源生态
位空缺(RackShift 停更后,consumer 端全是 Ventoy 类 U 盘工具);③特性
面真正的差距在验收测试的可执行半边/安全合规动作/发现自动化/AI
bring-up 组合件/资产生态输出——均为既有件组合,成本低。

## 9. netboot.xyz 对照(2026-11-24:发行版适配与引导链路定向调研)

> 调研对象:[netboot.xyz](https://github.com/netbootxyz/netboot.xyz)
> (Apache-2.0,12.6k★,iPXE 菜单聚合全发行版安装器,官网
> [netboot.xyz](https://netboot.xyz),自托管文档
> [docs/selfhosting](https://netboot.xyz/docs/selfhosting/))——§8.4
> 单行定性的深化,看它"适配这么多发行版"的组织方式与引导链路有无
> 可借鉴点。结论:生态位不重叠(菜单聚合器 vs API 引擎),适配组织
> 方式与 mammoth 同构,值得抄的是引导链路上的几个工程细节。

### 9.1 生态位:广而浅 vs 窄而深

| | netboot.xyz | mammoth |
|---|---|---|
| 本质 | 无状态菜单聚合器:静态 iPXE 脚本放 Web,任何人 chain 来选 | API 先行装机引擎:per-MAC 动态脚本+任务状态+BMC 闭环 |
| 引导时决策 | 人在机器前用菜单选发行版/版本 | 服务器按 MAC 查 netboot_entries 分派,机器前无菜单 |
| 应答文件 | 无(kickstart/preseed 仅运行时 URL 输入框) | 方言驱动生成,task-token URL 注入 |
| 内核/initrd | 上游镜像直拉,或自家 live_endpoint(squash 工件) | 内容寻址 ISO 解包池 |
| 部署形态 | 零后端,纯静态文件 | 常驻服务(proxyDHCP/TFTP/HTTP)或 external kit |

### 9.2 发行版适配机制:声明式注册表 + 构建期渲染静态菜单

四层结构,运行期零模板逻辑:

1. **releases 注册表**(roles/netbootxyz/defaults/main.yml):每发行版
   一键,name/mirror/base_dir/menu/enabled/versions——版本列表由维护者
   手填(renovate 提醒),无运行时版本发现;
2. **endpoints.yml**(仓库根):live/netboot 工件清单(path/files/
   os/version/flavor/arch/kernel),路径带 hash 锁版本,kernel endpoint
   (vmlinuz+initrd)可被多桌面 flavor 的 squash 复用;工件由自建
   流水线产出、发布到 GitHub Releases;
3. **每发行版一个手写菜单模板**(templates/menu/*.ipxe.j2,100+):
   按家族差异化——anaconda 系直接用上游镜像 images/pxeboot/ 拼
   `inst.repo=`,版本循环 Jinja2 构建期展开成静态 label;debian 系走
   上游 netboot/ 目录 + preseed/autoinstall URL 运行时输入,新 ubuntu
   走 Subiquity(squash 内核 + url=live-server ISO);live 系
   vmlinuz+initrd+filesystem.squashfs;工具系 memdisk/单 .efi;
4. **Ansible 构建期渲染成静态 .ipxe** 发布到 Web(site.yml):菜单更新
   =重发布,客户端 chain 每次拉最新脚本,引导器不用刷。

**对照结论**:适配组织方式与 mammoth OSDriver 注册制数量级相当
(Cobbler 条目"评估后不吸收"的同款判断再次成立:per-distro 参数是
家族模板的语义开关,数据化注册面是假性降本);版本来源 mammoth 更优
(ISO 登记自动探测 vs 手填清单);覆盖广度差距是十几年社区积累的量级
问题,不是架构问题。

### 9.3 引导链路与工程细节

链路:DHCP(next-server+filename)→ TFTP 拉 undionly.kpxe /
snponly.efi(自编译 iPXE,入口脚本编译期嵌入)→ cpuid 探架构 →
chain boot.cfg(全局变量+每发行版 mirror 变量+架构门控+SB 检测)→
version.ipxe 自升级检查 → 主菜单 → chain 子菜单 → kernel/initrd
上游直拉 → md5sum 打印 → boot。

值得记录的细节(标注 mammoth 对应面):

| 细节 | netboot.xyz 做法 | mammoth 对应面 |
|------|------|------|
| 运行时覆盖层 | local-vars.ipxe 在 boot.cfg 前 chain,`isset \|\| set` 只写默认值(rhel_base_url/live_endpoint/boot_timeout) | external kit 站点定制形态(待补) |
| 逐级验签 | imgverify 每级菜单对照 detached 签名,失败不执行;`iseq ${efi/SecureBoot} 01` 时自动降级关闭(SB 签名版 iPXE 无 imgverify 命令) | shim 链只验二进制,脚本/kernel 无验证——SB 降级先例适用 |
| proxyDHCP | use_proxydhcp_settings 决定用 proxy 还是 root DHCP 提供的 tftp | mammoth proxyDHCP 已有,客户端变量式选择可记 |
| 租约静态兜底 | `isset ${dhcp-server}` 不成立则把 iPXE 实际租约拼成 `ip=${ip}::${gateway}:${netmask} BOOTIF=${netX/mac}` | earlynet.go 静态注入按 NIC 清单,缺"iPXE 实际租约"一档 |
| 架构门控 | boot.cfg 按 arch/platform 置 menu_linux/menu_live 等标志显隐;云环境自动 console=ttyS0 | 服务端已等效(boot.arch 按 DHCP opt 93 分发 NBP) |
| 自定义菜单 | github_user→chain 用户 GitHub 仓库菜单;custom_url→任意 URL | 自定义 iPXE 逃生门候选(§8.2)同型参照 |

### 9.4 可借鉴点与处置(候选,未立项)

按优先级:

1. **iPXE 租约静态兜底——✅ 已落地(2026-11-26)**:渲染脚本对 pipeline
   发的 `ip=dhcp` token 自动展开为 iPXE 运行时分支(`leaseFallbackArgs`;
   用户 extra args 携带该 token 上游即拒,pipeline-owned 判据成立,零驱动
   改动):`isset ${dhcp-server}` 成立维持 `ip=dhcp`(零回归),否则用 iPXE
   已到手的租约拼静态参数(`ip=${ip}::${gateway}:${netmask}:::none` +
   DNS 可用时 `nameserver=${dns}`),netboot 装机与 enroll 零注册两路
   渲染器均生效。触发条件较调研时的表述做了修正:纯无 DHCP 段 iPXE
   到不了脚本(官方答案仍是 POOL/edge-pool),兜底救的是内核期二次
   DHCP 失败(PXE-class 过滤、snooping 限速);GRUB/shim 链与 preseed
   (netcfg 自管网络)不适用,未做。形态细节见 docs/operations.md §4.5.0。
2. **渲染脚本验签(P2 候选,安全)**——mammoth SB 链只验二进制
   (shim→grub),NBP 之后全 HTTP:渲染脚本与 kernel/initrd 可被同网段
   篡改。netboot.xyz 模式可抄:运行时私钥对渲染脚本签名,NBP 侧
   imgverify 验签(shim→grub 链用 grub hashsum);"SB 环境自动降级
   关闭"是同类兼容问题的现成答案(2288H shim 链降级 vMedia 的变体)。
3. **local-vars 覆盖层(P2 候选,external kit 打磨)**——kit 加可选
   chain 的 local-vars.ipxe(站点镜像源/超时),`isset||set` 默认值
   模式,引擎产物不动。
4. **零注册轻量菜单(可选)**——零注册脚本补极轻菜单(agent shell/
   本地盘/重试),§8.4"对照零注册 console 面"的展开;与 API-first
   定位有张力,待真实诉求再动。
5. **md5sum 打印(零成本)**——渲染脚本加一行 md5sum 内核/initrd
   校验和,现场排障省事。

**不借鉴**:引导器自升级(version.ipxe+chain 自替换——mammoth 的
NBP 每次引导由自家 TFTP 现发,天然更优);构建期静态渲染(无后端
产品的选择,mammoth per-MAC 动态渲染是正确的服务端形态)。
