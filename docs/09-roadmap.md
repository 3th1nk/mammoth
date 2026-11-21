# 09 · 演进路线

> **当前状态(2026-11-18)**:v1.5.0 已发布(承载机部署引擎镜像 v1.5.0-14)。
> M0~M7 全部交付;v1.x 迭代覆盖:agent initramfs 安装路径(dd/rescue/验收/
> 老化等 payload 的底座)、BMC 能力面(固件清单/BIOS 写/擦盘/健康+SEL/
> BIOS 基线)、**双厂商 KVM 控制台(Dell/H3C,粘贴注入)**、headless 无带外
> 形态、镜像工件库、rescue/装后验收/decommission 生命周期、观测面(syslog
> 通道/地址服务可观测)。
>
> 本文档是路线图:**已交付项压缩为台账**(细节看 git 历史与对应文档),
> 待办与候选按优先级排列,决策记录保留(避免重新论证)。2026-11-18 整理:
> 此前的逐项叙事(含各里程碑验收标准原文)见该次整理前的 git 版本。

## 已交付里程碑(M0~M7)

| 里程碑 | 交付 | 版本 |
|---|---|---|
| M0 骨架与通用带外 | OpenAPI 3.1 契约(oapi-codegen+gin)单二进制多模式入口;BMC 驱动接口 + Redfish/IPMI(电源/引导/虚拟介质);job/task 状态机;credential/machine 注册 | — |
| M1 盘查:规格级 | Redfish 盘查 → `machine.hardware`;厂商兼容矩阵框架(docs/compat/) | — |
| M2 盘查:分区级 | inband_ssh 探针 → layout 快照、版本化保留策略 | — |
| M3 首个发行版端到端 | Rocky 系 kickstart 驱动 + 介质重打包 + 五阶段流水线 + 批量 job | v0.1 |
| M4 保留分区 | 分区块级复用(keep 档) | v0.2 |
| M5 第二发行版与矩阵化 | ubuntu(autoinstall)/debian(d-i)驱动 | v0.3 |
| M6 运营完备 | 备份恢复、安全基线、契约冻结(边界定案 2026-09-18:就地落地) | v1.0 |
| M7 PXE/iPXE 网络引导 | 三方言四镜像真机全链(虚拟介质+PXE 双载体)、shim+grubnet Secure Boot、零注册入门(enroll/claim/固件门禁) | 2026-09 |

## v1.x 已交付台账(2026-09 ~ 2026-11)

### 安装通路与发行版

- **agent initramfs 安装路径**(v1.1;docs/12;qemu 双固件 + 真机闭环)——
  后续 dd/rescue/验收/老化 payload 的共同底座
- **镜像直写通路 dd**(2026-11-06 真机全链;2026-11-16 压缩镜像库识别)——
  `image.distro=dd` + format 三档(raw/raw.gz/raw.xz)+ 写入流哈希与读回
  逐字节验证 + CIDATA 槽 firstboot;封装规范见下文
- **windows 通路**(2026-09-19~24):unattend + SetupComplete 通路(qemu
  受理层验证)、agent apply-image 真机闭环 ×5 轮、用户脚本前后置接入
  (代码落地,验证随窗口)、渲染即拒边界(SupportNone 范式)+ spec 通用
  硬化(checksum 强制 / min_size_gb floor / 静默语义全改渲染即拒)
- **库引用装机源闭环**(2026-11-03,9eded37)——image.id + vMedia 组合
  (库 FilePath / http-ISO URL 对 anaconda 是死源;2288H dracut emergency
  定位):vMedia×kickstart 的 prepare 暂存池树 + PoolTreeURL,渲染三分支
  互斥(nfs 特判 / 池树 / http 真树直通),PXE 侧池树全覆盖;构建期门禁
  兜底(inst.repo=cdrom 即拒)。真树 URL 直通曾被 prepare 暂存误伤
  (目录索引当 ISO 必败),HTTPTreeSource 共享判定补齐。定案:池树暂存
  成本为定案(nfs-ISO loop-mount 流真机不稳定,7c0e751);库行不承载
  nfs:// 源(注册门禁 + fetcher 状态机,绕行 spec 内联 nfs:// 成本低)
- **headless 无带外形态**(2026-11-04 代码:protocol=none + enroll→claim +
  WoL;docs/13;2026-11-16 办公 PC 真机全链收口——enroll→claim→install 全通,
  operations §4.8.1)
- **装后软件源渲染 package_source**(2026-09-24,四方言)
- **发行版/复验矩阵**:rocky9 / ubuntu22/24/26 / debian13(双载体真机)、
  uniontechos(双通路真机复核,复验口径已改最新 ISO)、windows2019/2022/
  2025、kylin V11 / rocky10(vMedia UEFI)、22.04-crypt 复验(2026-10-28)

### BMC 能力面

- **四能力接口**:FirmwareInventory / BiosSetter(@Redfish.Settings PATCH,
  ETag If-Match)/ DriveEraser(NIST 800-88,#Drive.SecureErase)/
  Health+SELReader(电源态+传感器表 / SEL 只读 500 条)——两段式确认契约
  (confirm:true + runner 活表二次校验)沿 BIOS→擦盘→decommission 沿用
- **BIOS 基线模板 + 漂移巡检 + 批量扇出**(2026-11-07)
- **双厂商 KVM 控制台**(2026-11-18):引擎自研 `bmcweb/{id}/kvm`——
  Dell iDRAC6/7/8 走 avct/APCP 自研桥(DVC/ASpeed/文本模式解码 + USB HID
  注入),H3C HDM 走 RFB 直通桥(VNC 认证代填 + 消息过滤),**粘贴注入**
  (ClientCutText → US 键盘逐键,⌘V/Ctrl+V 直贴;真机 E2E 双代固件 +
  浏览器实测);华为仍走 OEM SSO 深链。定案与实录 docs/compat/{dell,h3c}.md

### 生命周期与运维

- **rescue mode**(2026-11-07,vMedia+PXE 双载体,agent 内存系统 + sshd;
  真机两轮已修复报告预算与 PXE 载体 plan 404,遗留全链闭环实录)
- **装后验收探针**(2026-11-07,acceptance_reports 判定 pass/degraded/fail,
  永不门禁)
- **decommission 生命周期动作**(2026-11-07,擦盘→证据采集→retired;
  复投 = 重新盘查)
- **镜像工件库**(2026-09-24;2026-11-16 压缩件解压算 raw 摘要/跨度落库)
- **装前硬件健康门禁**(2026-09-24,探针 smartctl/nvme 采集 +
  `policy.health_gate` off|report|block)
- **观测面**:安装器 syslog 通道(vMedia/PXE 双路,2026-09-24)、装机网段
  地址服务可观测 + edge 饥饿上报 + 提交预检(2026-11-07)、启动 DHCP
  探测自检(2026-11-20:plain DISCOVER 无 opt60——引擎应答门与
  OnObserve 天然静默,防自答与 pending 污染;POOL/PROXY/external 三形态
  分类矩阵,log + events 双通道,serve/edge 双挂点;operations §4.5.0)
- **PXE 工程**:外部 DHCP+TFTP 逃生门(`MAMMOTH_PXE_MODE=external`)、
  arm64 引导链(qemu SB 验证,投递层待 ARM 真机)、proxy 模式 UEFI NBP
  链选择(`shim|ipxe`)、agent PXE 载体 modloop 修复(4b75b7b;miniproot
  载体换代真机收口)、**配置面收敛**(2026-11-20:`MAMMOTH_PXE_ENABLED`
  并入 `MAMMOTH_PXE_MODE` 三值 builtin|external|off——4 态含死态收敛为
  3 态全有效;kit 导出解耦出 external,builtin+edge 混合拓扑解锁;池预留
  段守卫——静态 `ip=` 只写本段机器,跨段机器回落段内站点 DHCP)、
  **edge-pool**(2026-11-20,当日立项当日落地)——远段无站点 DHCP 的
  地址服务:引擎导出 armed/enroll MAC 清单端点
  (`/netboot/pool-manifest.json`,ETag = body hash),edge `--dhcp-pool`
  按清单门控发一体 OFFER(租约+引导)——决策留在引擎、执行搬到 edge,
  与 proxyDHCP per-MAC script 同构;新分配存活探测(ProbeAlive 提取共享,
  phantom 租约避让段内静态设备;顺带堵上引擎 enroll 路径的探测缺口);
  edge compose 补 NET_BIND_SERVICE(nonroot 绑 67 必 EPERM 的既有缺口)

### dd 镜像封装规范(构建方契约,保留)

dd 通路镜像须自带分区表与引导器,且**最后一个分区为 ≥16MiB 的 CIDATA
槽**(GPT type=0700 即可;agent 运行时 mkfs.vfat 覆盖后写入 NoCloud seed
三件:identity/network/access)。机器侧**不做任何分区表操作**——alpine
sfdisk 对大 MegaRAID 卷 append 确定性 EINVAL,内核自解析 GPT + `partx -a`
强扫是唯一可靠路径(2026-11-06 2288H 真机定案)。构建示例(任意宿主
parted):

    parted -s img.img mklabel gpt mkpart system 1MiB 31MiB mkpart CIDATA 31MiB 47MiB

交付格式推荐 `raw.gz`(busybox gunzip 内建零依赖);`raw.xz` 可选(运行时
apk add xz);无槽镜像 + seed 意图 = 任务失败(错误点名契约)。

## 下一阶段

### 立项未启动(按优先级)

1. **固件升级**(P2,Redfish UpdateService 写路径)——任务化固件下发
   (两段式确认 + 202 任务轮询 + 版本回读校验);前置:iBMC 对
   UpdateService 的支持度与固件包形态真机核验(华为/浪潮各一轮);
   风险高(变砖),缺能力 422 沿契约先例。
2. **TPM2 + LUKS sealed key**(P2,等保/信创合规刚需)——spec.storage 增
   密档意图(LUKS)+ TPM2 sealing(Clevis 渲染);发行版通路按方言分型
   (anaconda 原生声明 / autoinstall+preseek 走 late-commands),agent
   通路全自持;前置:2288H TPM 真机摸底。
3. **硬件老化/验收测试包 burn-in**(P1;MAAS testing scripts 同型)——
   验收探针之上的动作化扩展:`run_diagnostics` 动作,agent 内存态跑
   stress-ng/memtest/fio(GPU 机型 nvidia-smi),结果入
   acceptance_reports 同表;数据中心实践为上架必做 4~24h 分档;载体/
   互斥门复用 rescue 同型;验收 fake+qemu,不占真机窗口。
4. **BMC 凭证轮换**(P1;DMTF 安全实践)——`rotate_credential` 动作
   (Redfish ManagerAccount Password PATCH),新密码只写永不回显语义
   不变,两段式确认,批量走既有 job;时序注意改密后旧会话失效——先验
   新凭证可登录再落终态。
5. **发现规则/自动 claim**(P1;Foreman Discovery Rules 同型)——
   pending_machines→claim 的人工升格之上:claim 规则(按 MAC 前缀/
   机型/固件底座匹配→自动升格 + 赋标签/BIOS profile),纯策略层不动
   契约;与 BIOS 基线、批量扇出联动。
6. **硬件画像自动标签**(P1)——batch-labels 之上的规则化:盘查 schema
   简单谓词→自动打标(gpu/nvme-raid/10gbe 等);是批量扇出与 GPU 机群
   分组的前置。

### 候选组(条件触发,不排期)

- **P3(2026-11-07 立项)**:批量扇出编排(标签/分组 + 限速 + 失败熔断,
  前置 = 机器标签面);GPU/DPU 机型探针(lspci class/NUMA 入档案);装机
  容量轮(并发 vMedia 会话上限、数百台级压测);DPU 装机(MAAS
  BlueField-3 风向,等硬件)。
- **P2(2026-11-17 立项)**:AI/GPU 机群 bring-up 模板(GPU 健康面入盘查
  + burn-in 压测 + 驱动/CUDA 经 package_source+scripts 模板化);带内硬
  RAID(agent storcli payload);库存导出 NetBox DCIM(webhook 已有加
  适配器);自定义 iPXE 引导脚本逃生门;重置密码动作(rescue 同型
  one-shot payload)。
- **场景观察,不立项**:多站点管理面 / 白盒交换机 ZTP / 无盘常驻节点 /
  TPM 远程度量认证 / SEL 带外健康 watch / 周期带外巡检(异常→事件→
  webhook)——等真实需求触发(related-work §8.3)。
- **等条件组(专项,触发即做)**:
  - **UefiHttp**(Redfish HTTP Boot)——等多厂商真机(OEM URI 各异,
    单台华为验不出跨厂商);
  - **bootc/OCI 直装**——代码已落地自持(render/bootc 驱动 + agent
    `MAMMOTH_BOOTC` 分支 + 提交门),**提交入口已关闭**(distro=bootc
    即拒并指路 dd 通路);重开条件 = 持有 OCI OS 镜像流水线的真实客户
    出现;冻结面:qemu 全链轮(musl podman 跑 glibc 容器,podman #18398)、
    工件库 OCI 引用、CIDATA-via-mount;完整定案见 git 历史(2026-11-07
    复议:目标场景错位——信创 OS 无 bootc 镜像,部署前置比 dd 重一个量级);
  - **驱动注入机制**(2026-11-03 立项)——现状无 dracut --add-drivers /
    驱动盘(DD)机制,发行版通路依赖介质 inbox 驱动。候选形态按成本
    递增:①compat 矩阵记 known_defects(零代码)→ ②PXE 引导树外挂
    驱动目录 + %post dracut 重建 → ③安装器驱动盘注入(inst.dd,装前
    生效)。触发 = 真实机型因驱动缺失装机失败且载体切换不可行;
  - **openEuler 驱动接入**(2026-09-24 立项)——24.03 是 anaconda RHEL9
    代际,kickstart 与 rocky9 同方言,接入成本 = rocky9 家族驱动注册 +
    支持矩阵行 + 真机验证;随复验窗口顺带;
  - **iPXE sanboot HTTP 直挂 ISO**(2026-11-03 立项)——免任务级重打包,
    但 sanboot 引导的是 ISO 自带引导器(内核参数/应答无法注入),适用
    面 = rescue/live 介质、自带应答机制的原盘、vMedia 死路机型逃生门;
    UEFI 侧支持度未验证且 SB 不可用(裸 iPXE);触发 = 真实 rescue 场景;
  - **边缘地址服务实时门控**——fail-closed + TTL 负缓存设计已定稿
    (本文整理前版本);触发 = edge 出现"未登记 MAC 白吃地址"的真实
    问题;当前 dnsmasq allowlist 已覆盖第一层;
  - **共享二层地址治理**(邻机占址归属/装机段划段)——线下协调,与
    relay 回归绑 runbooks/test-baselines.md 基线表滚动执行。

### 真机窗口回归清单(已交付项的遗留验证,汇总)

> 2026-11-20 核实收敛:headless 真机全链(enroll→claim→install→WoL,
> operations §4.8.1)、dd 真机端到端(2288H 六轮 + 办公 PC 直写)、
> ubuntu26 确认门、UOS 双通路复核、kylin V11 / rocky10(vMedia)、
> agent PXE 载体(miniproot 换代)均已真机收口,移出本清单。
> 2026-11-21 增补实测:**.74 跨网段 vMedia 装机六阶段绿**(ubuntu24 库
> 引用,BMC 挂 .250 NFS ISO,`.221` 在线;HANDOFF §装机实测会话)——
> 顺带实证三条:iBMC PXE 固件发包缺陷(LOM1 DISCOVER UDP checksum 错,
> 内核丢包 → Linux DHCP 服务全收不到 → PXE-E18;**待 tcpdump -v 独立
> 复核**,修复方向 = edge raw 接收通道,候选)、console/KVM 与 boot
> 并发的 set_boot_device 412(修复 58d3831 待部署)、spec.network
> match.mac 必须跟实际接线口(LOM1=97,旧记 94 渲染 = 装后地址死)。

- relay 跨网段真机回归(等网络设备 ip helper 协调窗口;giaddr 应答已有
  单测,11-pxe-walkthrough §4;edge 跨网段形态已真机全链,仅 relay/giaddr
  形态待)
- Windows 用户脚本前后置 qemu/真机验证;package_source 四方言真机回归
- 健康门禁探针 smartctl/nvme 采集核验;BIOS 漂移巡检真机扇出(BiosSetter
  活读/写形态 2288H 已实录);decommission 真机轮(擦盘→证据采集→retired;
  DriveEraser 对 LSI 卷支持度已有真机结论)
- rescue 全链闭环实录(真引导已真机实证,两轮真机修复报告预算与 PXE
  载体 plan 404 后,报告→释放未实录);验收探针 qemu+真机轮
- 复验轮:rocky10-PXE 复核 / kylin V10 受限项(静态网 NM 卡死)/
  22.04-crypt 的 bond 声明(等 eno2 接线);2288H 单机轮装顺序覆盖
  (22/24/rocky9 已证明可行)
- arm64 投递层真机(等 ARM 机器;SB 签名链 qemu 已验,投递层与 x64 共享
  代码 + opt 93=11 单测覆盖)
- 附带:LITEON 目标盘换线后复测窗口保留(dd 直写硬件边际,operations
  §4.8.1)

## 长期方向(不承诺排期)

- 跨机器 Raid/LVM 拓扑编排(单机 storage.lvm 已落地,docs/04 §5.1.1;
  剩余为跨机器拓扑面)
- gRPC 内部面间协议(当前队列 + DB 足够)
- **redfish over 169.254**(host↔BMC 带内通道)——服务器 BMC 经
  NCSI/共享网卡与宿主 OS 存在链路本地直连(Dell iDRAC 169.254.0.1、
  华为 iBMC 169.254.95.118 等);形态 = `probe: auto` 降级链又一臂,
  无独立管理网部署(边缘机房/单机交付)下带内直连采集盘查/健康/SEL;
  前置 = 真实需求 + 各厂商链路本地地址与凭证边界摸底(入 compat 矩阵);
  只作增强通路,永不作注册主路径
- 引擎池树升格一等 apt 源(带签名元数据;apt-ftparchive + 签名密钥管理,
  设计后议)——现状建议 `MAMMOTH_UBUNTU_APT_MIRROR` 指向带 InRelease
  的本地镜像(aptly/reprepro)
- 发布者真实性:镜像库注册期 minisign 签名验证(dd 写盘流校验已覆盖
  完整性,发布者真实性属注册期;纯 Go ed25519 可实现),随 imaging
  工作流立项

## 已评估并排除的方向(决策记录,避免重新论证)

- **官方 CLI / Terraform Provider(2026-11-07 评估后不做,用户定案)**——
    竞品扫描曾立项(当时 MAAS/RackN 均有 provider,视为 API-first 标配
    生态件);复议:客户端面已由 MCP server + OpenAPI 契约覆盖,AI 调用方
    (本项目的重点调用形态)走 MCP,IaC 调用方暂无真实需求;Terraform
    provider 属独立仓生态工程,维护面 > 边际收益。重开条件:IaC 化调用方
    真实出现。

- **~~办公电脑批量安装~~(2026-09-19 评估 → 2026-11-04 真实需求触发,
  分场景重审定案)**——2026-09-19 曾以三面理由整体排除(范式不匹配/
  无 BMC 契约缺口/消费硬件长尾)。2026-11 工控机 POC(真实客户场景)
  触发当年预留的重审条件,拆分定案:
  - **纳入:Linux 全新安装(范围 A)**——工控机/信创桌面/办公 Linux
    机器:标准发行版 + 声明式配置,installer 范式与引擎灵魂一致;缺的
    仅是 headless 机器形态(当初预留的触发器场景,真实需求已现)。
    Linux inbox 驱动覆盖实证充分(工控机网卡/存储无外置驱动依赖)。
    立项 = headless 机器形态(设计草案 docs/13-headless-design.md,
    估四周末级)——**代码面已实施(2026-11-04,feat/headless 分支,
    实施映射见 13 §8),剩余真机/qemu 验证轮**;
  - **维持排除:Windows/应用镜像克隆(范围 B)**——imaging 范式
    (镜像制作/分发/增量/还原)与"声明式意图→安装器现场执行"的
    产品灵魂错配,FOG/MDT/还原卡为成熟领地;镜像管理与软件分发
    永久留给上层(分层原则不变);
  - **按单评估:Windows 裸装(无应用定制)**——unattend 通路已有,
    消费硬件 WinPE 驱动注入是长期黑洞,不作为承诺能力,单客户需求
    出现时单独评估;
  - 附带收益兑现:headless 模型与 WoL 同时服务**服务器场景的无 BMC
    机器**(工控机之外,blade/特殊机型同理)。

- **发行版接入声明化 distros.json(2026-09-18 评估后收敛)**——曾全量
  落地(12 发行版声明化),评估后同日 revert 收敛为类型化 Go 注册表:
  接入成本在模板 Go + 真机验证而非注册面(三方言连环修为证),声明
  文件是假性降本。保留收益:`distros[].family` 契约导出、ubuntu 架构
  修正(amd64-only)。完整决策记录见 docs/06 §6;池能力建模(boot_pool
  vs system_pool)结论留在 docs/12 §6 备用。

- **MediaBaseURI 拆旋钮 / vMedia 换协议(2026-11-20 评估后维持现状)**——
  该 base 同时派生 BMC 面的 media_uri(vMedia/rescue/probe,客户端是
  BMC,管理网段)与安装器面的 casper nfsroot(客户端是 dracut,装机
  网段),部署须让同一 NFS 服务端可达两段(内核 nfsd 导出 MediaDir
  定案;nfsx 对华为 iBMC 不兼容,compat/huawei.md §6;.5 边缘节点只承担
  PXE 的 dnsmasq/TFTP)。评估:两消费面同根(同一 MediaDir 的不同子
  目录)、同协议(NFSv3)、同服务端,单导出面即同时满足;跨段可达 =
  一次性路由配置(跨网段实测定案);拆旋钮(独立 nfsroot base)收益
  仅限"两段异构 NFS 服务端"场景,成本 = 配置/文档/测试面 +1,且
  windows 通路 SMBShare 独立旋钮先例确立"协议不同才拆"的边界;
  HTTPS 直挂超主力机型(iDRAC6/7/8、iBMC 6.41)能力面,进程内 NFS
  已实测否定。曾计入「已知缺口」属归档错位(2026-10-27 入册时性质为
  部署注意记账);部署操作与排障清单以 operations §4.7 为权威。

## 版本策略

- M3 = v0.1;M4 = v0.2;M5 = v0.3;M6 = v1.0(2026-09-18 就地落地:契约
  仅新增演进,破坏性变更进 v2 讨论)
- v1.1.0(2026-09-19):agent initramfs + BMC 能力三部曲 + PXE 逃生门 +
  arm64 链路 + uniontechos
- v1.2.0 ~ v1.5.0(2026-09~11):dd 镜像直写、rescue/装后验收/BIOS 基线/
  decommission 生命周期、镜像工件库(压缩识别)、观测面、headless、
  双厂商 KVM 控制台——契约"仅新增演进"原则全程未破
- 发布流程:`git tag vX.Y.Z && goreleaser release --clean`(amd64/arm64,
  版本与 commit 经 ldflags 注入)
