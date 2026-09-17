# 09 · 演进路线

> **当前状态(2026-09)**:M0~M6 全部交付(契约冻结 = v1.0,**边界定案
> 2026-09-18:就地落地,见"下一阶段"题注**);M7 PXE/iPXE 网络引导通路已
> 交付并**真机闭环**,三方言四镜像全链打通——rocky9(虚拟介质 + PXE)、
> ubuntu22/24(casper NFS 载体,零人工重装 + 重启自举双验证)、debian13
> (d-i netboot 载体,2026-09-17 真机全绿),均为 2288H V5;ramdisk 探针
> PXE 已 succeeded;shim+grubnet Secure Boot 闭环;零注册入门与设备档案
> 全链交付(option 93 观测、enroll/pending_machines/claim、固件门禁)。
> SQLite 最小部署形态已评估并放弃(见 10 §D2),存储收敛为 PostgreSQL-only。
> 余项:uniontechos(见 compat/distros.md)、relay 真机回归、arm64 引导链
> (见 M7 余项与下一阶段 2)。

里程碑按"每阶段交付物独立可用"的依赖关系排序。M1 之前没有任何东西能对用户产生价值,
因此 M0 的唯一目标是让最通用的能力先跑起来。

## M0 · 骨架与通用带外能力 ✅

- 仓库脚手架:OpenAPI 3.1 契约(oapi-codegen + gin)、CI(契约 diff 检查)、单二进制多模式入口
- 可观测埋点框架:slog 标准字段集、Prometheus 指标、OTel 边界埋点(no-op 默认)
- 容器发布:多阶段构建出 `mammoth` / `mammoth-builder` 双镜像,docker compose 一键拉起
- BMC 驱动接口 + Redfish/IPMI 两个实现:电源、引导设备、虚拟介质、KVM URL
- `credential` / `machine` 注册,`POST /machines/{id}/actions` 全量动作
- job/task 最小状态机(表队列,契约测试套件于 CI 的 PG 集成作业执行),心跳 + interrupted 判定

**验收**:纯 API 完成一批机器的开关机/重启/挂载介质,进程重启后 running 任务被正确
标记 interrupted 且可重试。

## M1 · 盘查:规格级 ✅

- Redfish 盘查 → `machine.hardware`;呈现规则(RAID 卷优先)与 `coverage: partial` 标注
- 盘查动作(`probe: redfish`)、厂商兼容矩阵框架(`docs/compat/`)
- `layout` 资源结构定义落库(暂无数据源)

**验收**:注册机器后 30s 内返回完整规格视图;BMC 凭证错误得到分类错误码。

## M2 · 盘查:分区级 ✅

- `inband_ssh` 探针(只读命令集、单连接、超时隔离)→ layout 快照
- 快照版本化与保留策略;`GET /machines/{id}/layout`
- `probe: auto` 组合策略

**验收**:对一台运行中的机器采集出与人工 `lsblk` 一致的分区快照;带内不可达时得到
明确错误而非超时悬挂。

## M3 · 首个发行版端到端安装 ✅(v0.1)

- 发行版驱动接口 + Rocky/RHEL 系驱动(kickstart 方言)
- 渲染层:storage(wipe 档)/ network(静态/bond)/ identity / access / scripts
- 介质策略:发行版原盘重打包为任务引导介质(`boot-<token>.iso`,应答文件离线烘入)
- install 流水线五 stage 全通;批量 job(concurrency / on_task_failure)

**验收**:三台异构机器一批次安装 Rocky,网络按 bond 声明生效,root 口令按任务随机生成;
批次内单机失败不阻塞其余机器,失败机器可单 task 重试。

## M4 · 保留分区(分区块级复用)✅(v0.2)

- `keep: disk` / `keep: partitions` / `preserve` 全语义
- `%pre` 校验脚本生成 + LAYOUT_DRIFT 中止回报
- 快照漂移检测默认开启(`policy.verify_layout`)
- 提交侧校验:快照缺失/不命中在提交时拒绝

**验收**:双盘机器重装,系统盘重建、数据盘分区原样保留且挂载不变;人为改动数据盘分区表
后重试,任务以 LAYOUT_DRIFT 显式失败,数据无损。

## M5 · 第二发行版与矩阵化 ✅(v0.3)

- Ubuntu(autoinstall)驱动,含保留分区的 partial 支持声明与提交时拒绝
- 支持矩阵文档化;`KeepPartitionSupport()` 语义接入提交校验
- ramdisk 探针(可选启用)——**✅ 真机闭环(2288H V5,2026-09-13)**:
  alpine 虚拟介质载体,LSI RAID 卷可见,discover 端到端 succeeded;
  PXE 通路属真实网络环境阶段

## M6 · 运营完备 ✅(v1.0)

- ✅ Webhook 事件投递(HMAC-SHA256 签名、类型过滤、退避重试)
- ✅ 审计/事件查询 API(resource/type 过滤 + 游标)+ 事件 SSE(job 级 + 全局)
- ✅ 软 RAID(anaconda raid 行)与硬 RAID(Redfish Volume)声明式配置(六阶段流水线)
- ✅ CLI(cobra + goreleaser 分发):批量注册、盘查、安装提交、进度跟踪
- ✅ install-plan 试算端点(只读解析 V1,`POST /machines/{id}/install-plan`)
- ✅ docs/operations.md(备份恢复)、docs/security-baseline.md(安全基线)
- ✅ task_logs 表与检索 API(日志双写落库,`GET /jobs/{id}/tasks/{taskId}/logs`,
  reaper TTL 默认 90d;stage 耗时直方图此前已埋)
- ✅ ramdisk 探针(alpine 虚拟介质载体,`POST /render/{token}/probe-report`
  + discover `probe=ramdisk` 集成;qemu BIOS+UEFI 双模式闭环 + 2288H 真机
  端到端 succeeded)

## M7 · PXE/iPXE 网络引导通路 ✅(2026-09)

- ✅ 引导策略抽象:`spec.boot.strategy`(virtual_media | pxe,提交时声明,
  部署默认 `MAMMOTH_BOOT_STRATEGY`);`bootStrategy{prepare,arm,release}` 在
  provision 层切开,virtual_media 原样包裹既有序列,bootStage 与 probeRamdisk
  的重复引导序列随之收敛;
- ✅ netboot 服务(借鉴 Pixiecore proxyDHCP):proxyDHCP(UDP 67/4011,旁路
  应答,永不分配地址)+ 最小 TFTP(仅 NBP)+ iPXE 二段链(BIOS/UEFI;二进制
  go:embed 内置,PROVENANCE 全程可溯,Debian ipxe 2.0.0+dfsg-5);
- ✅ 机器面 `/netboot/script?mac=`(按 MAC 渲染 iPXE 脚本;无条目回 `exit`
  脚本回固件引导序)+ `/netboot/files/{token}/{file}`(引导树,allowlist);
- ✅ RHEL 系安装走 PXE(`inst.repo=nfs:` 复用 nfsx 导出,零新增安装源工作);
  ubuntu/debian 当时提交即拒(PXESupport none 门禁),PXE 化后放开为 full
  (见下);
- ✅ 可选 DHCP 池(`MAMMOTH_PXE_DHCP_POOL`):无站点 DHCP 的机房,UEFI PXE ROM
  拿不到租约就不会走网络引导——mammoth 对 PXE 客户端与装机内核兼做全量
  DHCP(租约内存态);有站点 DHCP 时保持纯 proxy 模式;
- ✅ **真机闭环(2026-09-13,2288H V5)**:四跳(DISCOVER 广播 OFFER→TFTP
  ipxe-amd64.efi→HTTP 脚本→kernel/initrd 200)→ dracut 池租约 → anaconda
  NFS 装机 → 完成回调 → verify_ready SSH 命中,六阶段全绿;
- ✅ ramdisk 探针 PXE 化(`probe=ramdisk boot=pxe`):alpine 引导树 + overlay
  第二段 cpio 追加进 initramfs,modloop 走 HTTP——M5 遗留的"PXE 通路"余项
  就此关闭;
- ✅ UEFI x64 Secure Boot 链(shim+grubnet):shimx64.efi(Microsoft 签名)
  → grubx64.efi(Debian 签名 grubnet)→ TFTP 动态渲染 grub.cfg-01-<mac>
  → HTTP 拉 kernel/initrd;x64 NBP 从 ipxe-amd64.efi 切到 shim 链
  (DHCP 无法区分 Secure Boot,UEFI x64 统一走 shim);真机闭环
  (2026-09-14,2288H);
- ✅ `netboot_entries` 注册表 + 引导项生命周期(注册/宽限注销/孤儿清扫),
  `MAMMOTH_PXE_ENABLED`(默认关,启用时 bind 失败即退出)、
  `MAMMOTH_PXE_NEXT_SERVER`、`MAMMOTH_BOOT_STRATEGY`;
- ✅ ubuntu/debian PXE 真机闭环(2026-09-17,2288H):debian13 = d-i
  netboot 载体 + 签名 HTTP 池(池钥匙经 debootstrap 进 target,连环修
  十一项,零人工六阶段全绿);ubuntu22/24 = casper NFS squashfs 源
  (连环修十八项:BOOTIF 钉口、内核参数分号转义、crypt 密码、spec 静态
  网络翻译 ip= 等,零人工重装 + 重启自举双验证)——实录见
  compat/distros.md 与 compat/huawei.md;
- ✅ 寻址策略三层定案(2026-09-17):**spec 静态声明 > 池预约 > DHCP**;
  池模式三件套——租约白名单(只应答已武装装机任务的 MAC)、白名单
  nil-entry 门、ReserveFree 预约探测(ping+邻居表,ICMP 黑洞型静默占址
  也能识别);完成回调以 RemoteIP(非 XFF,防伪造)回写 machine.ssh_address;
- ✅ 装机通路工程收尾(2026-09-17):netboot 内置安装器日志 syslog sink
  (514/udp,租约反查落 task_logs)+ debian `syslog=` 内核参数;共享池
  内容寻址缓存(`MediaDir/pool-store/<sha256>`,多任务复用一份解包树);
  TFTP OACK 容忍(X722 shim 不回 ACK0,短等待后直接落 DATA,~10s/文件
  自愈 → ~2s);apt 信任收尾(debian 池行显式 signed-by、双方言容忍清除);
- 余项(不阻塞):UefiHttp(Redfish HTTP Boot,厂商 OEM URI 各异)、
  外部 DHCP+TFTP 逃生门、**真机 relay 回归**(跨 VLAN:应答回 giaddr:67
  已实现并有单测,带 relay 的真机/qemu 拓扑未跑,见 11-pxe-walkthrough §4)、
  arm64 PXE 引导链(ipxe-aa64.efi / shim+grubnet aa64,
  opt 93 = 0x000B;无真机,后续——信创混合机群 x86_64/ARM64 混部的刚需,
  与 kylin/uniontechos 矩阵绑定排期;option 93 固件观测入档案可先行,
  见下一阶段 2)。

## 下一阶段(v1.0 后,按优先级)

> **v1.0 边界定案(2026-09-18,方案 A:就地落地)**:M6 四项门槛早已全部
> 达成,实际交付已远超门槛(三方言四镜像真机全链 + Secure Boot + 零注册),
> 当前 HEAD 即 v1.0。曾一度把 tag 延后至"PXE 增强 + BiosSetter 完成",复议
> 时其前提(ubuntu/debian PXE 未闭环)已消失,BiosSetter 的 BIOS 前置动机
> 也被 `SetBootDevice(PXE, oneshot)` 现有路径覆盖——UefiHttp、外部
> DHCP+TFTP 逃生门、BMC 能力接口、Windows unattend、arm64 引导链全部外移
> v1.x,不阻塞冻结。

1. **BMC 能力接口**(v1.1 主体):BiosSetter(根治 BIOS 前置)→ FirmwareInventory
   (固件基线核对)→ NIST 800-88 擦盘合规(见 related-work);高危动作引入
   **两段式确认契约**(请求显式确认标志 + 服务端二次校验),做成 API 策略
   开关供全自动化调用方关闭;
2. **PXE 增强余项**(v1.1):UefiHttp(Redfish HTTP Boot,厂商 OEM URI 各异)、
   外部 DHCP+TFTP 逃生门(mammoth 不能当 PXE 服务的部署形态)、arm64 引导链
   (ipxe-aa64.efi / shim+grubnet aa64,opt 93 = 0x000B;信创混合机群刚需,
   无真机前 option 93 固件观测先行积累);~~ubuntu/debian PXE 化~~ ✅ 已入
   v1.0(2026-09-17 真机闭环);
3. **发行版扩展**:Windows unattend → uniontechos(blocked,等 UOS 支持,
   不主动排期);~~ubuntu 24.04~~ ✅ 已入 v1.0(现有驱动直接可用,已入
   回归基线 runbooks/test-baselines.md);
4. **agent initramfs 安装路径试点**(中期,related-work §1 修正结论):自有
   最小 agent(分区 + 从池装内核/包),六阶段流水线与声明式 spec 原样承载,
   preseed/kickstart 方言降级为兼容模式——与"方言抽象的价值在兼容存量,
   不应阻碍自持安装路径"的修正呼应;先试点再定去留;
5. **发行版接入声明化**(related-work §4,Cobbler 式):distro 签名(载体
   内核/initrd 相对路径、内核参数、bootloader 形态)收敛为数据文件,接入
   新发行版从写 Go 变成写声明;与 4 联动评估落地顺序;
6. **例行回归**(维护窗口,不占版本边界):CentOS7/Kylin/rocky10-PXE/
   22.04-crypt 复验轮、真机 relay 回归(跨 VLAN,giaddr 应答已有单测)、
   共享二层地址治理(线下协调)——绑定 runbooks/test-baselines.md 基线表
   滚动执行。

## 长期方向(不承诺排期)

- 多机 Raid/LVM 拓扑编排
- gRPC 内部面间协议(当前为队列 + DB,足够)

## 版本策略

- M3 为 v0.1(第一个可用版本);M4 为 v0.2;M5 为 v0.3;M6 为 v1.0
- **v1.0(2026-09-18 定案:就地落地)**:门槛四项——契约冻结(仅新增演进)、
  双发行版端到端、备份恢复文档、安全基线审查——2026-09 前全部达成;落地时
  的实际交付已远超门槛:三方言四镜像真机全链(虚拟介质 + PXE 双载体)、
  shim+grubnet Secure Boot、零注册入门、装机通路工程收尾(syslog sink /
  共享池缓存 / OACK 容忍 / apt 信任收尾)。曾一度把 tag 延后至"PXE 增强 +
  BiosSetter 完成",2026-09-18 复议取消(前提消失、动机被现有路径覆盖),
  边界外移项见"下一阶段"。
- **v1.1 方向**:BMC 能力接口(BiosSetter 起步,两段式确认契约随行)+
  PXE 余项(UefiHttp、外部 DHCP+TFTP 逃生门)+ Windows unattend;契约
  仅新增演进(向后兼容字段/端点),破坏性变更进 v2 讨论。
- 发布流程:`git tag vX.Y.Z && goreleaser release --clean`(amd64/arm64,
  版本与 commit 经 ldflags 注入)。
