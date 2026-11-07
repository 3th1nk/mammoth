# 09 · 演进路线

> **当前状态(2026-09)**:M0~M6 全部交付(契约冻结 = v1.0,**边界定案
> 2026-09-18:就地落地,见"下一阶段"题注**);M7 PXE/iPXE 网络引导通路已
> 交付并**真机闭环**,三方言四镜像全链打通——rocky9(虚拟介质 + PXE)、
> ubuntu22/24(casper NFS 载体,零人工重装 + 重启自举双验证)、debian13
> (d-i netboot 载体,2026-09-17 真机全绿),均为 2288H V5;ramdisk 探针
> PXE 已 succeeded;shim+grubnet Secure Boot 闭环;零注册入门与设备档案
> 全链交付(option 93 观测、enroll/pending_machines/claim、固件门禁)。
> SQLite 最小部署形态已评估并放弃(见 10 §D2),存储收敛为 PostgreSQL-only。
> 余项:relay 真机回归;uniontechos 已真机闭环(2026-09-19,1050u2a 复验
> 随窗口);arm64 引导链
> 本机可验部分已闭环(2026-09-18:SB 签名链 qemu 验证 + 源码比对,见
> 下一阶段 4),投递层与端到端回归待 ARM 真机。

里程碑按"每阶段交付物独立可用"的依赖关系排序。M1 之前没有任何东西能对用户产生价值,
因此 M0 的唯一目标是让最通用的能力先跑起来。

## M0 · 骨架与通用带外能力 ✅

- 仓库脚手架:OpenAPI 3.1 契约(oapi-codegen + gin)、CI(契约 diff 检查)、单二进制多模式入口
- 可观测埋点框架:slog 标准字段集、Prometheus 指标、OTel 边界埋点(no-op 默认)
- 容器发布:多阶段构建出 `mammoth` / `mammoth-builder` 双镜像,docker compose 一键拉起
- BMC 驱动接口 + Redfish/IPMI 两个实现:电源、引导设备、虚拟介质(华为 VmmControl
  OEM 已实装);KVM URL 未实装——真机定案裸路径直开不可用(启动依赖 Web UI
  流程),SSO token 直链待二期(07-bmc §5、compat/huawei.md)
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
- ✅ RHEL 系安装走 PXE(`inst.repo=nfs:` 复用 MediaBaseURI 的 NFS 导出,零新增安装源工作);
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
  池模式三件套——租约白名单(只应答已登记装机任务的 MAC)、白名单
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
>
> **顺序按真机可得性重排(2026-09-18)**:当前仅一台真机(2288H V5),qemu/
> fake 可先行项前置(agent 试点/BMC 读接口/声明化/逃生门/arm64 链路),
> 强真机或外部条件依赖项显式隔离进第 9 项,不占当前序列。
>
> **2026-09-24 增补(同赛道产品调研收敛)**:第 2 项追加健康面读能力
> (HealthProvider + SELReader);新增第 6~8 项(镜像工件库 / 装后软件源
> 渲染 / 装前硬件健康门禁)——全部 qemu/fake 可先行,不占真机窗口;
> 明确不跟进:对外镜像源站、虚拟化管理、常驻带内故障感知、RBAC(超出
> 单二进制引擎边界)。
>
> **2026-11-07 增补(竞品扫描)**:新增第 11~19 项(rescue mode / 装后
> 验收探针 / BIOS 基线巡检 / CLI·Terraform / 固件升级 / bootc 直装 /
> TPM-LUKS / 生命周期收尾 / P3 候选组)——调研底稿见
> docs/related-work.md §7。

1. **agent initramfs 安装路径试点**(related-work §1 修正结论,提前:qemu
   全程可验且架构红利最大——减少未来所有发行版接入对真机联调的依赖面):
   自有最小 agent(分区 + 从池装内核/包),六阶段流水线与声明式 spec 原样
   承载,preseed/kickstart 方言降级为兼容模式;试点不碰真机,结论决定
   去留与所需 API 形态;
2. **BMC 能力接口**(v1.1 主体,按风险拆序):~~**FirmwareInventory**~~
   **✅ 已落地(2026-09-18)**——`FirmwareInventoryProvider` 可选能力接口
   (driver.go,VolumeCreator 同型)+ redfish 实装(UpdateService→
   FirmwareInventory 宽容 walk,取数器注入纯函数可测)+ fake 脚本化
   (FirmwareList 字段 + FailOps)+ discover 采集落 machines.firmware
   (迁移 00008)+ 契约只追加 `machine.firmware`(gen 再生,ipmi 缺能力
   即无数据盘查不失败);qemu 冒烟全链(注册→盘查→API 出固件)。
   **BiosSetter ✅ 已落地(2026-09-18,fake 先行,真机回归随 2288H 窗口)**——
   `BiosSetter` 可选能力(Bios 属性表读 + @Redfish.Settings 设置对象
   PATCH:ETag If-Match、读改写保 pending、202 任务轮询;redfish 实装 +
   fake 脚本化);两段式确认契约定稿(高危动作范式,NIST 800-88 沿用):
   请求 `confirm:true`(默认策略缺省 422 BIOS_CONFIRM_REQUIRED 不建 job,
   MAMMOTH_BIOS_CONFIRM=optional 供全自动调用方关第一道)+ runner 活表
   二次校验(永在):未知属性整体拒 BIOS_ATTRIBUTE_UNKNOWN、同值 no-op
   不下发、只写真差集;`GET /machines/{id}/bios` 活读端点;契约追加
   ActionSetBiosAttributes / BiosView / Capabilities.bios_set_confirm;
   冒烟全链绿。
   **NIST 800-88 擦盘 ✅ 已落地(2026-09-18,fake 先行,真机窗口随
   BiosSetter 回归顺带)**——`DriveEraser` 可选能力(逐盘 Redfish
   `#Drive.SecureErase`,serial 先全量解析再下发、绝不擦一半,202 任务
   轮询;redfish 实装 + fake 脚本化,默认支持可脚本关闭);`erase_drives`
   动作沿两段式范式:请求 `confirm:true`(缺省 422
   DRIVE_ERASE_CONFIRM_REQUIRED,MAMMOTH_ERASE_CONFIRM=optional 关第一道)+
   runner 活盘表二次校验(永在):未知 serial 整体拒
   DRIVE_SERIAL_UNKNOWN、all=true 展开活表、重复 serial 去重保序;
   `GET /machines/{id}/drives` 活读端点(serial 即擦除身份);
   `task.drive_erase` 事件携带 erased serials + 厂商上报擦除机制
   (NIST 800-88 sanitization record 底稿);契约追加
   ActionEraseDrives / DrivesView / Capabilities.drive_erase_confirm;
   单测(fake/redfish/provision)+ acceptance 冒烟全绿(422 门禁 /
   未知 serial 拒 / confirm 走通 / 事件落库)。真机回归:iBMC 对 LSI 卷
   的 secure erase 支持度未知,随把握窗口顺带测。
   **健康面 HealthProvider + SELReader —— ✅ 已落地(2026-09-24,纯读
   能力,fake 先行,真机随 2288H 窗口)**——可选能力接口 ×2(driver.go,固件清单同型):
   `Health`(电源态 + 传感器表:风扇 RPM / 温度 / 电源瓦数 / 电压,各带
   ok|warning|critical 态;overall 取 worst-of,chassis Status 一并计入,
   absent/unknown 不拖累)+ `SELReader`(System Event Log 只读:
   Managers→LogServices→Entries 宽容 walk,created 倒序,截断最近 500 条);
   redfish 实装(取数器注入纯函数可测)+ fake 脚本化(SensorList/SELList
   默认 plausible 集);契约新增两个活读端点 `GET /machines/{id}/health`、
   `GET /machines/{id}/sel`(沿 bios/drives 同步读先例:422 缺能力 /
   502 BMC 错,无持久化无轮询);IPMI 不实现——goipmi 命令栈无 SDR/SEL,
   缺能力即端点 422,真机全走 redfish(先例:固件清单);周期带外巡检
   (异常→事件→webhook)等真实需求触发再排,见第 9 项。
3. ~~**发行版接入声明化**~~ **✅ 已评估并收敛(2026-09-18)**——曾全量
   落地 distros.json(12 发行版声明化),评估后同日 revert 收敛为类型化
   Go 注册表:接入成本在模板 Go + 真机验证而非注册面(三方言连环修为
   证),声明文件是假性降本。保留收益:`distros[].family` 契约导出、
   agent 包集参数留驱动内、ubuntu 架构修正(amd64-only,live-server 无
   arm64 介质)。完整决策记录见 docs/06 §6;池能力建模(boot_pool vs
   system_pool)结论留在 docs/12 §6 备用;
4. **PXE 余项的 qemu 可验部分**:~~外部 DHCP+TFTP 逃生门~~ **✅ 已落地并
   qemu 同型验证(2026-09-18)**——`MAMMOTH_PXE_MODE=external`:mammoth
   零 UDP 绑定(纯 HTTP 面),站点 dnsmasq 承担真 DHCP+TFTP,静态 kit
   (NBP+grub 模块+两枚蹦床+dnsmasq 模板)启动时导出到
   MediaDir/netboot/external-tftp;客户端自报身份缝合动态决策(iPXE
   ${net0/mac} → /netboot/script?mac=、grub ${net_default_mac} → 新增
   /netboot/grub/<mac> HTTP 端点,与 builtin TFTP 渲染同源);SB 链保持
   (shim/grub Debian 签名不变);代价:option 93 观测失效(enroll 不受
   影响)。docker 网桥+dnsmasq+UEFI guest 经外部链完成 alpine agent 全装
   (六阶段绿),harness scripts/pxe-dev/external-e2e.sh;部署文档
   operations.md §4.5.1。
   **arm64 引导链(2026-09-18 落地:资产+代码+SB 链 qemu 验证,投递层单测+同构外推,真机待补)**——
   Secure Boot 链对齐 x64:nbpARM64 从 unsigned ipxe-arm64.efi 切
   shimaa64.efi(Microsoft 签名)→ grubaa64.efi(Debian 签名 grubnetaa64
   改名)+ grub/arm64-efi/*.lst 模块清单(shim-signed/grub-efi-arm64-signed/
   grub-efi-arm64-bin trixie 三包,与 x64 pinned 同版本;fetch 脚本扩展双
   arch + bin 包,PROVENANCE 补录);external kit 同步导出 arm64 链,
   dnsmasq example 增 efi-aarch64 标签(client-arch 11);builtin
   proxyDHCP 的 opt 93=11 路径同源切换。**qemu 验证了 SB 签名链**:
   AAVMF secboot + VARS.ms(Secure Boot ON)从磁盘引导 shimaa64 →
   验签 grubaa64 → GRUB 2.12 运行(Debian 官方固件,harness
   scripts/pxe-dev/arm64-sb-chain.sh)。**投递层(UDP 67/69)在 qemu
   aarch64 上不可验——上游架构边界:ArmVirtQemu.dsc 明言
   "NETWORK_SNP_ENABLE is IA32/X64/EBC only",arm64 虚拟固件没有
   UEFI PXE 栈**(SNP/UNDI 是 x86 生态产物);该层与 x64 共享代码,由
   单测(opt 93=11→shimaa64 断言)+ x64 真机链同构性覆盖;实现正确性
   经上游源码比对(shim 二阶段命名/grub per-MAC cfg 查找序/RFC 4578
   arch 11/Ironic·MAAS·boots 的 arm64 映射)逐点核对。
   **真机回归待 ARM 机器**。信创混合机群刚需;~~下一项 NIST 800-88~~
   ✅ 已落地(见第 2 项,2026-09-18 fake 先行)。
   ~~ubuntu/debian PXE 化~~ ✅ 已入 v1.0(2026-09-17 真机闭环);
5. **Windows 用户脚本(spec.scripts 前后置接入)**——**✅ 代码落地(2026-09-23,方案按下述定案;契约仅新增 ScriptSpec.shell=cmd|powershell 缺省 cmd;渲染即拒边界齐备;qemu/真机验证待下一窗口)**——windows 驱动现状对
   spec.scripts 渲染即拒("user scripts are not supported yet (SetupComplete
   is engine-owned)",setup/agent 两通路同,render/windows/windows.go:206、
   :292):SetupComplete.cmd 每镜像仅一个,是完成回调 + 静态网绑定的引擎
   专属槽位,用户脚本抢占即断回调链。**结论:前后置都能支持,槽位不同**:
   - **post_install(装后首启,SYSTEM 登录前)= SetupComplete 托管拼接**:
     引擎前缀(按 MAC 绑网)+ 用户段按序执行(逐段捕获退出码)+ 引擎
     完成回调永远收尾——单槽位变引擎托管多段,回调链结构上不可断;失败
     语义扩展 task.json 契约(回调携带 status=failed + 段退出码);备选
     补充 = unattend oobeSystem FirstLogonCommands(原生多槽有序,但时点
     更晚、登录后才跑,只做补充不做主槽);
   - **pre_install(装前)= 按通路分型**:setup 通路注入 WinPE 阶段
     (startnet.cmd 引擎前置段后拼接,或 unattend windowsPE pass
     RunSynchronous 原生多槽;cmd 为主,PowerShell 需向 boot.wim 注入
     WinPE 可选组件,代价大不作默认);agent 通路由 alpine agent 脚本体
     天然承载,但语义是 busybox/Linux 环境——适合盘级操作,Windows 语义
     脚本(bat/ps1)不适用,契约需显式声明脚本运行语境;
   - **分区操作的边界(照非空 mount 收窄先例)**:装前 WinPE diskpart、
     装后 PowerShell Storage cmdlets 都是原生机制,但与声明式 storage 是
     双真相——声明式管辖的启动盘脚本不得触碰(schema 校验拒),脚本只
     管声明式表达不了的边角(如装后建数据卷);
   - 验证:qemu 应答受理层可先行(winpe 引导/语言页/回调链先例,
     compat/distros.md §windows);真机回归随已闭环的 windows 真机通路
     (agent apply ×5 轮,2026-09-22)。
6. **镜像工件库(image registry)**——**✅ 代码落地(2026-09-24;真机
   无涉,acceptance 冒烟全绿)**(同赛道产品调研收敛为"工件管理 + 源指配"
   两小块,不做对外镜像源站)——镜像注册
   (`POST /images`:source_url + sha256 必填 + 发行版元数据可选)→
   后台拉取队列落 `MediaDir/images/<sha256>.iso`(内容寻址,同 sha 多
   注册共享同一文件,下载中重注册直接挂同队列)→ `spec.image.id` 引用
   注册件(与 `source` 互斥,提交校验"二选一"),引用即校验:镜像必须
   ready,注册 sha256 反哺既有 spec.image.checksum / EnsureISOVerified
   门禁通路( spec 另带 checksum 时两者一致才放行);images 自带状态机
   (fetching/ready/failed + 事件 image.ready/image.failed,自带 fetch
   worker,不进机器 job 模型——webhook 投递器先例);删除清行,缓存
   文件在无其他行引用时回收;
7. **装后软件源渲染(package_source)**——**✅ 代码落地(2026-09-24,
   四方言渲染测试全绿;真机回归随窗口)**(2026-09-24 立项,离线机房
   刚需;渲染层 identity/access/scripts 同型扩展)——
   `spec.package_source.repos[]`(name/url/gpg_key_url/suite/components,
   suite 缺省取发行版代号 jammy/noble/trixie)按方言渲染:anaconda
   `repo` 指令(装机期)+ %post 落 /etc/yum.repos.d(装机后,gpgcheck
   按 key 有无)、autoinstall late-commands 落 deb822 sources.list.d
   (key 由安装器环境 python3 取到 /target keyring,无 key 渲染
   Trusted: yes)、d-i post-install 脚本落 sources.list.d(busybox wget
   取 key,无 key 渲染 trusted=yes)、windows 渲染即拒(SupportNone
   先例);name 字符集 + URL scheme 提交校验(name 成文件名,字符集即
   注入边界);
8. **装前硬件健康门禁**——**✅ 代码落地(2026-09-24,fake/PG 可验面
   全绿;真机探针采集随 alpine 载体工具链窗口核验)**(2026-09-24 立项,
   固件门禁模式同构延伸 + ramdisk 探针通路复用)——探针追加磁盘健康采集:启动期
   best-effort 自引导介质自身 /apks 仓安装 smartmontools/nvme(--no-network,
   载体带则装、不带则跳过,启动零风险),逐盘采集 smartctl -H
   overall-health / NVMe critical_warning,快照 disks 项附 health:
   pass|fail(工具缺失、设备不报一律留空,不假装健康)→ probe-report
   落档(机器最近一次探针健康快照);提交侧 `policy.health_gate`
   (off|report|block,默认 off):block 档在最近探针报告存在不健康盘
   时 422 HEALTH_GATE_FAILED(带 serial 名单,探针报告缺失不拦——
   门禁只对有据可查的坏盘拦截),report 档只落档不拦截;
9. **等条件组(不排期,条件触发)**:
   - **UefiHttp**(Redfish HTTP Boot)——等多厂商真机(OEM URI 各异,单台
     华为验不出跨厂商);
   - **Windows unattend**——**v1 代码面就绪(2026-09-19,见 compat/distros.md
     windows 节)**:windows2019 驱动(autounattend.xml + SetupComplete.cmd
     wimlib 注入)+ builder windows 布局家族(El Torito 重放 + UDF);虚拟
     介质 + UEFI-only + Standard Core,回调式 verify 零改动。qemu 已验证
     winpe 引导与应答受理层(2019/2022 镜像均在 248)。
     **2026-09-20 真机定案**:2288H(iBMC 6.41)虚拟介质 UEFI 引导对
     windows 介质固件级失败(原版/重打包 ×NFS/客户端重定向 全灭,
     同通路 linux 介质正常;完整证据链 compat/huawei.md windows 虚拟介质
     节)。**通路路线定策**:①**v1.x 主线 = windows PXE 载体落
     wimboot-over-HTTP**(builder 提取 bootmgfw/BCD/boot.sdi/boot.wim
     四件套,wimboot GPL2 纳管同 shim/grub 先例;网络引导绕开 El Torito,
     根治本固件缺陷,外部逃生门即投递载体);②**终局 = agent
     apply-image**(wimlib apply + 预烤 BCD + unattend 落 Panther,复用
     agent 引导与声明式落盘)——**✅ 真机闭环 ×5 轮(2026-09-22,compat/distros.md §windows
     agent apply-image 节)**:boot.installer=agent 契约/渲染/载体/runtime/
     投递面全链;首版手工 BCD 预烤在 specialize 撞 0xC0000098(NT 拒载
     手工 store,真机定案)→ 改**两段式**(第二段 wimboot WinPE 由
     bcdboot 原生生成)后六阶段全绿;boot.installer=auto 自动通路判定
     落地(runbooks/windows-agent-apply.md);~~遗留 = cancel 释放条目/机器
状态机回 ready 两个小修~~ **✅ 已修(2026-09-23)**:cancel/终态释放改先重读
DB 任务记录(claim 快照没有 prepare patch 的 token/strategy,陈旧解析误路由
virtual-media 导致条目与 boot tree 全漏);ramdisk 探针成功与 verify_ready
成功两处回置机器 ready(探针开转换不收口是停留 discovering 的根因);PG 回归
套件(terminal_release_test.go)+ test-pg 目标扩到 provision 包;~~同机
多轮重试堆积 pending 任务~~ **✅ 已修(2026-09-24)**:提交门 409
JOB_MACHINE_BUSY——install 任务针对同机存在 pending/running/interrupted
前驱时整单拒绝(契约 CreateJob 增 409,明细带阻塞任务/作业标识),先显式
cancel 旧作业再重跑;用户脚本(spec.scripts 前后置)接入已单列,见第 5 项;
     ③Ventoy 式 grub 链载 = 可选介质侧实验;
     ④iBMC 升级 = 正确修复(与 SecureErase 缺失叠加升级动机),物理 USB
     = 有人场景最短路径。
     **spec 覆盖度全量评估(2026-09-23,对 windows 驱动逐语义核对)**:
     声明式主干(image / storage wipe 档 / 静态网 / identity / access 三件)
     真机闭环,能力边界靠渲染即拒守住(keep:disk / keep:partitions /
     preserve / RAID / bond·vlan / scripts → SupportNone)。缺口三层:
     ①~~**静默语义丢失**~~ **✅ 已改渲染即拒(2026-09-23,与 raid/scripts/
     keep 同一范式)**——非启动盘 partitions(planDisks 开机盘判定后二次
     扫描)、access.ssh_keys(RenderAnswers 双通路门禁,Server Core 无
     sshd)、mount:"swap"(移除 mount 校验豁免)三处静默面全部显式拒绝,
     边界用例入 TestRenderWindowsBoundary;②~~**通用缺口**~~ **✅ 已实现
     (2026-09-23,provision 通用层,全发行版受益)**——spec.image.checksum
     经 builder.EnsureISOVerified 强制(sha256,不符即拒 + 缓存驱逐 +
     path:size:mtime 备忘避免跨阶段重复哈希,六个 spec 取数点全切换;
     EnsureISO 本体不动,引擎资产照旧);storage.root_device_hints.
     min_size_gb 作为选择器池 floor 进两个 resolver(共享
     diskMatchesSelector 谓词,未知容量不触发 floor,install-plan 同语义);
     alignment 枚举校验后为 advisory(anaconda/curtin/WinPE 原生对齐,
     不转发,注释与 validateStorageExtras 定案);③**
     能力边界(等真实需求触发再排)**——edition 选择(现硬编码
     SERVERSTANDARDCORE,windows.go:144)、RAID、bond/vlan。
   - **库引用(image.id)× anaconda 方言的 inst.repo 缺口(2026-10-27 新环境实录,待修)**——
     kickstart 驱动把 ImageSource 原样拼进 inst.repo:库引用流解析出的本地 FilePath 与
     http-ISO URL 都是 anaconda 不收的形态(仅 nfs:host:/path.iso / 真树 URL / cdrom 可用,
     nfs 特判已有)。本轮 nfs:// 源绕行真机闭环(源盘驻 .5 nfsd);正式修法 = prepare 阶段
     派生 nfs repo 形态并(按需)经 relay 中继源盘,或库行直接承载 nfs 形态源。
   - **MediaBaseURI 单旋钮双消费者注意(2026-10-27)**——该 base 同时派生 BMC vMedia 的
     media_uri 与 casper/安装器的 nfsroot:部署形态必须让**同一 NFS 服务端**同时可达
     两者(引擎主机内核 nfsd 导出 MediaDir 为定案;nfsx 对华为 iBMC 不兼容,见
     compat/huawei.md vMedia 节;.5 边缘节点只承担 PXE 的 dnsmasq/TFTP)。
   - **复验轮**——~~22.04-crypt~~ **✅ 已闭环(2026-10-28,.250 环境九轮
     真机:静态网 + root_password 全链通过,顺带修复 late-commands 裸
     /target 失效 / root 密码 $ 展开 / netplan 缺 network: 包装三层,
     见 HANDOFF 同名条目)**;余项 = Kylin/rocky10-PXE + 22.04-crypt 的
     bond 声明顺带轮(等 eno2 接线);2288H 单机轮装顺序覆盖
     (22/24/rocky9 已证明此模式可行);
   - **openEuler 驱动接入(候选,2026-09-24 立项)**——两篇 openEuler/
     Kylin PXE 实战文章核认:openEuler 24.03 是 anaconda RHEL9 代际
     (pykickstart/anaconda 36),kickstart 与 rocky9 同方言,UEFI grub
     引导流与既有机制同型——接入成本 = rocky9 家族驱动注册 + 支持矩阵
     行 + 真机验证,无新方言工作。注意点:引擎渲染 `rootpw --plaintext`
     (一次性随机),不涉 openEuler 默认 SM3 散列(那是 `--iscrypted`
     手写 ks 的坑);驱动名建议 `openeuler24`。随真机窗口与复验轮顺带;
   - **驱动注入机制(候选,2026-11-03 立项,传统 PXE 实践对照确认的缺口)**——
     现状:无 dracut --add-drivers / 驱动盘(DD)机制,发行版通路依赖介质
     inbox 驱动,agent 通路明确"仅 inbox 驱动机型"(04-install-spec §5.4
     边界);缓解靠载体选型(ramdisk/agent 载体 lts 内核 + modloop 全量
     存储驱动、PXE 用 alpine NETBOOT tarball 规避 standard ISO initramfs
     缺机房网卡驱动,05-inventory 注)——但发行版安装器通路
     (kickstart/preseed/autoinstall)对带外特有阵列卡/新网卡无解。
     候选形态(按成本递增):①compat 矩阵把"驱动缺失机型"记为
     known_defects(装机失败时提示载体切换,零代码);②PXE 引导树外挂
     驱动目录 + kickstart 系 %post dracut 重建 initramfs(装后自愈);
     ③安装器驱动盘注入(anaconda inst.dd / dracut DD 进 initrd,装前
     生效,prepare 阶段构建)。触发器:真实机型因驱动缺失装机失败且
     载体切换不可行,届时从①起步逐档评估;
   - **启动时 DHCP 探测自检(候选,2026-11-03 立项)**——netboot/edge(原 edge)
     启动时主动发探测 DISCOVER,按应答分类本网段地址服务形态(站点 DHCP
     有/无/多个、应答是否携带引导选项),据此校验模式配置并给出可操作告警:
     池模式撞站点 DHCP = 双地址权威租约打架;proxy 模式无站点 DHCP = 客户端
     必然拿不到引导信息(PXE-E53 类引导即死)。与"装机网段地址服务可观测"
     (本文件独立定稿节)互补:那节管运行时 per-MAC 观测,本项管启动时配置
     自检(NBpxeserver 同类能力佐证价值)。触发:随 edge 跨网段真机
     回归(relay 回归)窗口顺带;
   - **proxy 模式 UEFI NBP 链选择**——**✅ 已落地(2026-11-03,单测全绿,
     真机回归随 edge 窗口)**——上游硬边界:grub2 不处理 UEFI 模式的
     Proxy DHCP 消息(RackN KB-00043、RH Bugzilla #1809246),shim→grubnet
     链在"edge + 不可控站点 DHCP"拓扑下必然落 GRUB 提示符(失败
     签名:shim/grub TFTP 成功 → 静默)。新增 `MAMMOTH_PXE_UEFI_NBP=
     shim|ipxe`(builtin)与 `edge --uefi-nbp`(默认 shim):ipxe =
     未签名裸 iPXE(ipxe-amd64/arm64.efi,external kit 同步补导出 arm64
     件),绕开 grub2 缺陷,SB 须关;SB-on + 不可控站点 DHCP = 行业无解
     (走 vMedia/UefiHttp 或谈判站点 DHCP option 66/67,部署边界与谈判
     配置落 docs/operations.md §站点 DHCP 共存节,验证矩阵落
     runbooks/edge-validation.md);全 DHCP 形态(池/external)永不
     命中该缺陷,保持签名链默认;
   - **iPXE sanboot/httpdisk 直挂 ISO(候选,2026-11-03 立项)**——HTTP 直挂
     原盘 ISO 引导(iPXE sanboot http://…),免任务级重打包;硬前提 =
     文件面 HTTP Range/ETag(池树通路已具备,2026-11-03 落地)。能力边界
     先行说清:sanboot 引导的是 ISO 自带引导器,内核参数/应答文件无法注入
     ——这正是重打包载体存在的理由,故适用面仅限 rescue/live 介质、自带
     网络寻址应答机制的原盘、以及 vMedia 固件死路机型的又一逃生门
     (compat/huawei.md windows vMedia 节那类);UEFI 侧 iPXE sanboot 支持
     度未验证且 SB 链不可用(裸 iPXE,wimboot 同款例外)。触发:真实需求
     (rescue 场景或 vMedia 死路机型复现)再排;
   - **agent PXE 载体 modloop 加载缺口(2026-11-04 真机暴露,✅ 代码落地,
     复验待窗口)**——edge 跨网段真机首次把 agent PXE 载体跑到
     storage 即四连 identical 失败(看不到盘):NETBOOT initramfs 精简
     features(无 HBA 驱动),modloop 后置加载依赖 openrc hwdrivers
     coldplug,coldplug 却跑得比 modloop 挂载早(qemu megasas 复现定案;
     vMedia 载体的 ISO initramfs 内建全量驱动,结构上免疫——09-19 的
     agent 真机闭环为 vMedia 载体,PXE 载体 agent 通路此前从未真机到过
     storage)。修复 = bootstrap 重跑 coldplug + 控制器 modprobe 兜底 +
     自包含诊断行随 bail log 回传(commit 4b75b7b)。**同窗口ubuntu26
     载体 subiquity 确认门未定案**(autoinstall 数据已加载、early_commands
     在跑,门仍弹;待装机环境 /proc/cmdline 实证,已排除 DHCP 因素);
     复验 = edge 拓扑重跑 alpine(装后 .221)+ ubuntu26;
   - **真机 relay 回归**——等网络设备配 ip helper 的协调窗口(giaddr 应答
     已有单测,见 11-pxe-walkthrough §4);
   - ~~**uniontechos**~~ **✅ 已闭环(2026-09-19 真机,见第 2 项)**;剩余
     1050u2a 复验与 PXE 复核随下一窗口顺带;
   - **共享二层地址治理**(邻机占址归属/装机段划段)——线下协调;复验与
     relay 回归绑 runbooks/test-baselines.md 基线表滚动执行;
10. **镜像直写通路**——设计已定稿(见下方「镜像直写通路」节,2026-11-06):
    dd 运行时 = agent initramfs 第三 payload,dd 优于 url=(内存墙),解压
    契约按 busybox 能力分三档;qemu 全程可验,切片①③不占真机窗口。
11. **rescue mode(救援动作)**(P1,2026-11-07 竞品扫描立项)——商业裸金属
    云标配(Latitude.sh/phoenixNAP:内存 RAM OS + SSH 修复),自研引擎普遍
    缺失;mammoth 全件已有:alpine agent RAM OS(bootstrap/plan 机制)+
    双载体(PXE/vMedia)+ console 代理 + syslog sink。形态 = 机器级动作
    `boot_rescue`(非破坏性,不进两段式确认):引擎渲染 rescue payload
    (agent 以 rescue 模式启动:sshd + 注入 ssh_keys,不执行任何安装步骤)
    → 按装机同型载体引导 → 机器报告 rescue ready(任务/事件落档,console
    链接可用)→ 运维在内存系统内排障 → 既有 power 动作重启回原系统,载荷
    随任务终态释放(dd applied 即终态的释放链先例)。契约:ActionBootRescue /
    事件 task.boot_rescue / 救援期间同机装机互斥(409 复用
    JOB_MACHINE_BUSY);alpine ISO 复用已注册介质,零新增资产。验收:fake
    BMC + agent 断言(sshd 起、无安装步)+ qemu 全链轮,不占真机窗口。
    **✅ 代码落地(vMedia 切片,2026-11-07)**——契约(ActionBootRescue +
    JobType rescue + 动作端点 409)/ 迁移 00012 / 单阶段 rescue flow
    (建载体 → vMedia 挂载引导 → 等 rescue-ready 报告 → 载荷即刻释放,
    机器留内存系统,超时按探针语义关机)/ agent `MAMMOTH_RESCUE=1` 分支
    (池装 openssh → authorized_keys → sshd → report ok → 前台 console
    shell 不 reboot;池缺席诚实降级 console-only)/ 回调 handler 按 flow
    分流(/render/{token}/complete 复用)/ 忙门双向
    (ActiveBlockingTasksByMachines flow IN install+rescue)/ docs 07 §6.4、
    03 动作清单、12 载荷分型。单测(渲染/agent 断言/上下文解析)+ PG 忙门
    (rescue↔install 双向)+ acceptance §3.6(契约面 + 409 + 终态诚实性)
    全绿。**✅ PXE 载体切片落地(2026-11-07 同日)**——双载体统一重构:
    载体决策纯函数(headless 强制 PXE / boot:"pxe" 显式覆盖 vMedia 死路
    机型 / BMC 缺省 vMedia),PXE 路径 = BuildAgentNetboot 引导树 + per-MAC
    注册(Kind rescue)+ 计划经 mammoth_base 走 context answers,arm/release
    全量复用策略层(pxe().arm 内含 headless 证据等待:带内重启/WoL/人工
    上电;releaseBootPayloadFresh 按 boot_strategy 路由);headlessActionGate
    对 boot_rescue 放开;载体决策单测;契约增 boot 字段。**遗留**:qemu
    全链轮(真引导 → agent 报告 → 释放全链)随窗口。
12. **装后验收探针(第七阶段)**(P1,2026-11-07 立项;MAAS testing scripts
    同型)——六阶段证据化流水线的收尾:装机完成回调后,引擎在下一次带内
    可达时执行验收采集(带内 SSH 凭证通路已有:网络连通 / 分区落盘对照
    spec 布局 / 内核版本 / 主机名与时区 / 关键服务健康)→ 机器验收报告
    (落档 + 事件)。与 verify_ready 的关系:verify_ready 是装中门禁
    (探针快照),本项是装后终验;ubuntu26 确认门这类悬案可借同一采集面
    读 /proc/cmdline 定案。验收:qemu + 真机各一轮。
    **✅ 代码落地(2026-11-07)**——采集器 `===SYS===` 段(host/kernel/
    cmdline/timezone/os/uptime,splitSections 重构为位置扫描,任意可选段
    鲁棒)+ 迁移 00013(acceptance_reports,滚动窗口 10)+ verify_ready
    同采集出报告(不增加第二轮 SSH)/ 判定 pass|degraded|fail(mismatch
    fail,unavailable degraded,**永不门禁**)/ 读面
    `GET /machines/{id}/acceptance`(无报告 = `{"report": null}`)+
    `task.acceptance_reported` 事件 / docs/06 §5 落档。单测(SYS 采集/
    报告构建四态)+ PG 套件(repo/端点)全绿;qemu/真机轮随窗口。
13. **BIOS 基线模板 + 漂移巡检**(P1,2026-11-07 立项;Metal3
    HostFirmwareSettings 企业形态)——BiosSetter 单机写之上:①机器档案
    存基线 profile(声明式 BIOS 属性集);②巡检动作对照活读 BiosView 与
    基线出漂移报告(事件 + 档案标记);③批量下发 = 按 profile 扇出
    ActionSetBiosAttributes(两段式确认契约不变)。纯既有能力组合,
    无新 BMC 面。**✅ 代码落地(2026-11-07)**——迁移 00014(bios_profiles
    + machines.bios_profile_id SET NULL)/ CRUD 面(POST/GET/DELETE
    /bios-profiles,重名 409,空集 422)/ 赋值(PUT machines/{id}/bios/
    baseline,profile 存在性校验)/ 漂移读(GET machines/{id}/bios/drift,
    provision.CompareBiosDrift = runner 写语义同一实现:matched/drifted
    [声明,活]/unknown;无赋值 null;machine.bios_drift_checked 事件,
    档案标记 = 事件面不设陈旧列)/ 扇出(POST /bios-profiles/{id}/apply,
    一个批量 power 作业,两段式门禁原样,显式 targets 或赋值集)/
    Machine 契约增 bios_profile_id。PG 套件全绿;真机漂移读随 2288H
    窗口顺带。
14. ~~**官方 CLI / Terraform Provider**~~ **已评估不做(2026-11-07 用户
    定案)**——客户端面已由 MCP server(`mammoth mcp serve`,README 有
    接入例)+ OpenAPI 契约(唯一事实源)覆盖;Terraform provider 是独立
    仓的生态工程,对单二进制引擎的边际收益低。真需求触发(IaC 化调用方
    出现)再复议;详见「已评估并排除的方向」。
15. **固件升级(Redfish UpdateService 写路径)**(P2,2026-11-07 立项;
    Ironic firmware_updates / Metal3 HostFirmwareComponents 同型)——只读
    FirmwareInventory 之上的自然延伸:任务化固件下发(两段式确认沿
    BIOS/ERASE 先例;202 任务轮询;版本回读校验)。前置:iBMC 对
    UpdateService 的支持度与固件包获取形态真机核验(华为/浪潮各一轮);
    风险高(变砖),缺能力 422 沿契约先例。
16. **bootc/OCI 镜像直装**(P2→**✅ 代码落地后冻结进「等条件组」
    (2026-11-07 复议定案)**;RHEL 10 Anaconda `bootc` 指令确立"OCI 镜像
    即装机单元"。**复议否决项**:立项纯趋势跟随、无真实需求触发(对比
    dd=工控机 POC、headless=办公电脑场景);**目标场景错位**——mammoth
    用户群(私有化/离线机房/信创/工控)与 bootc 生态(RHEL 系公有
    registry/在线拉取)错位,信创 OS 无 bootc 镜像;部署前置比 dd 重一个
    量级(podman 必配 + registry 基础设施 + ≥8G RAM,容器存储 tmpfs 是
    dd 内存墙的同款);增量薄弱(镜像化交付 dd 已通,声明式 agent 通路
    已通,免构建金镜像 = 另一种镜像工厂)。**重开条件:持有 OCI OS
    镜像流水线的真实客户出现**。已落地代码自持保留(独立驱动、渲染即
    拒、不碰现有通路;nocloud 归一重构独立有效)——qemu 轮/工件库 OCI
    引用/CIDATA-via-mount 随冻结一并搁置)

    **定案(agent 通路第四 payload,bootc.dev 官方文档核实)**:

    1. **通路开关** = `image.distro: "bootc"`(驱动注册表分发,dd 同款);
       `image.source` = OCI 引用(`docker://registry/repo:tag` 或裸
       `registry/repo:tag`,**推荐 digest 钉死 `@sha256:...`**——OCI 摘要
       即完整性,pull 即校验,spec.image.checksum 不适用);
    2. **storage 退化为单目标盘选择器**(dd 同款):`bootc install to-disk`
       自建布局(DPS GUID:root+ESP+BIOS boot,filesystem 由镜像 install
       .toml 或 --filesystem 定)——引擎不声明分区;lvm/raid/keep 互斥
       提交即拒;
    3. **运行时**(`bootc install` 必须在目标镜像自己的容器里跑,官方
       文档明文):agent `apk add podman`(alpine community 包;扩展池
       须携带或 alpine_repo 指向在线镜像——离线机房 ops 前置)→
       `podman pull <ref>` → `podman run --rm --privileged --pid=host
       --ipc=host -v /var/lib/containers:/var/lib/containers -v /dev:/dev
       --security-opt label=type:unconfined_t <ref> bootc install to-disk
       <target>`;`--root-ssh-authorized-keys`(access.ssh_keys 原生通道)+
       `--karg`(console 等);
    4. **first-boot 意图**(dd CIDATA 机制复用):to-disk 落盘后追加
       16MiB CIDATA 槽(dd 切片②同一机制),NoCloud 三件承载 identity/
       network/access(root_password 经 CIDATA)——**镜像须含 cloud-init**
       (文档写明);网络意图 netplan 双平面同 dd;
    5. **applied 即终态**(dd 同款):to-disk 完成 → report applied →
       载荷释放 + 终态;install.go 的 appliedTerminalDD 门泛化为双标记
       (MAMMOTH_DD_MODE=1 ∨ MAMMOTH_BOOTC=1);
    6. **RAM 预算**:容器存储走 tmpfs,base 镜像 1-2G + podman 足印
       ~200MB → **推荐 ≥8G 内存机型**;工控机/小内存边缘是 dd 的领地,
       bootc 不假设小内存(目标盘自缓存为后续优化,等真实需求);
    7. **qemu 全链轮必验**:musl podman 跑 glibc bootc 容器
       (podman #18398 是已知坑源,conmon 相关);
    8. **切片**:①契约(distro=bootc 驱动注册 + source OCI 形态校验 +
       storage 单盘互斥)+ render plan;②agent runtime bootc 分支;
       ③prepare 双载体(vMedia/PXE 同 dd)+ appliedTerminal 泛化;
       ④qemu 轮(随窗口)+ docs;镜像工件库承载 OCI 引用为后续。
    **✅ 代码落地(切片①②③,2026-11-07)**——render/bootc 驱动(OCI 引用
    形态校验:docker:// 剥离、ISO/路径形拒;**切片1意图边界:仅
    access.ssh_keys,hostname/network/password 渲染即拒**——bootc 自建
    布局无 CIDATA 槽,sfdisk 追加在 MegaRAID 有既录死墙,CIDATA-via-mount
    切片重开)+ nocloud 抽为共享包(render/nocloud,dd 委托,CIDATA 双消费
    契约归一)+ agent `MAMMOTH_BOOTC=1` 分支(目标盘解析=dd 习语 → apk
    podman(池必配,boot 行无条件 alpine_repo)→ pull → 容器内 to-disk →
    --root-ssh-authorized-keys 原生通道;applied 报告,无读回哈希——digest
    钉死即完整性)+ prepare 双载体泛化(vMedia 池无条件/PXE bootc 分支)+
    appliedTerminalDD 泛化双标记(MAMMOTH_DD_MODE ∨ MAMMOTH_BOOTC)+
    validateBootc 提交门(checksum 拒/单盘 wipe/分区与 lvm 拒)。
    渲染/agent/提交门单测全绿;**冻结项(随重开条件一并)**:qemu 全链轮
    (musl podman 跑 glibc bootc 容器,podman #18398 坑源必验)、镜像
    工件库 OCI 引用、CIDATA-via-mount。
17. **TPM2 + LUKS sealed key 装机**(P2,2026-11-07 立项;调研确认无竞品
    做成一等公民;等保/信创合规刚需)——spec.storage 增加密档意图(LUKS)
    + TPM2 sealing(Clevis 渲染);发行版通路按方言渲染能力分型(anaconda
    已有加密盘声明;autoinstall/preseed 靠 late-commands),agent 通路
    全自持。前置:2288H TPM 真机摸底。
18. **release/decommission 生命周期动作流**(P2→**bootc 直装之后的下一
    项**,2026-11-07 立项;MAAS 释放 / RackN Day-2 同型)——机器终态语义
    (退役/待重投)+ 动作流编排:擦盘(NIST 800-88 已有)→ 固件基线核对
    (FirmwareInventory 已有)→ BIOS 复位 → 资产证据报告导出 → 标记
    retired。全件已有,缺编排与机器状态语义。
    **✅ 代码落地(2026-11-07)**——迁移 00015(jobs.type +=
    decommission / machines.state += retired / decommission_reports 表)+
    三阶段 flow(全盘 NIST 800-88 → 证据采集[擦除记录 serials+mechanisms
    / 固件清单 / SEL 尾部 / BIOS 快照,各自 best-effort]→ retired)+
    eraseDrives 返回擦除记录(上下文承载,事件面不变)+ 确认门复用擦盘
    策略(**confirm 是唯一门禁,擦盘不可回滚**)+ retired 语义
    (install/rescue/decommission 一律 422 MACHINE_RETIRED;**复投 =
    重新盘查**,discover 状态机自带回 ready,零新端点)+ 批量提交原生
    (POST /jobs targets 机群一次退役)+ 报告读面
    GET /machines/{id}/decommission(无报告 = null 非 404)。
    **边界**:BIOS 复位不做(Redfish 无标准 reset-to-default,OEM 领地,
    退役档案里的 BIOS 快照即审计对照物)。PG 套件(确认门/忙门三向/
    retired 门/报告往返/复投)全绿;真机轮随 2288H 窗口(DriveEraser
    对 LSI 卷的支持度已有真机结论)。
19. **P3 候选组(2026-11-07 竞品扫描,条件触发)**——批量扇出编排(按
    标签/分组扇出 + 限速 + 失败熔断;前置 = 机器标签面);GPU/DPU 机型
    探针(lspci class / NUMA 入档案,驱动预装经 package_source+scripts
    已可达;AI 机群趋势,等真实机群);装机容量轮(并发 vMedia 会话上限、
    数百台级压测);DPU 装机(MAAS BlueField-3 风向,等硬件)。

## 长期方向(不承诺排期)

- 多机 Raid/LVM 拓扑编排(单机 storage.lvm 已落地——新建卷组与 keep:vg
  重装保留,docs/04-install-spec.md §5.1.1;剩余为跨机器拓扑面)
- gRPC 内部面间协议(当前为队列 + DB,足够)
- **redfish over 169.254(host↔BMC 带内通道)**——服务器 BMC 经
  NCSI/共享网卡与宿主 OS 间存在链路本地直连(常见 169.254.0.0/16,
  如 Dell iDRAC 169.254.0.1、华为 iBMC 169.254.95.118),OS 内不经
  管理网即可直达 Redfish。对 mammoth 的形态:带内探针侧复用现有
  redfish driver,作 `probe: auto` 降级链的又一臂——无独立管理网部署
  (边缘机房/单机交付)下,盘查/健康/SEL 可经宿主 OS 内直连采集,
  注册仍走常规管理面。前置:真实需求触发(无管理网场景)+ 各厂商
  链路本地地址与凭证边界摸底(放入 compat 矩阵);风险:该通道固件
  默认开关不一,只作增强通路,永不作注册主路径。

- **vMedia 安装器 syslog 通道(观测面)✅ 已落地(2026-09-24,按本节定案)**——
  ①渲染层:kickstart 系 `inst.syslog=`、casper/autoinstall 与 d-i
  `syslog=`(host 照抄 preseed 的 AnswerBaseURL hostname 模式,抽共享
  `render.SyslogHost`),vMedia 与 PXE 两路 KernelArgs 都带;②sink 归因:
  `netboot.IPRegistry`(provision 在 prepare 注册 spec 声明静态地址、TTL=
  任务预算+1h、release 路径 Forget)作租约链之后的回退,`serveSyslog`
  先租约后注册表——**归因边界:spec 静态地址可归因,site-DHCP 动态地址
  仍仅记源 IP**(无人声明的 IP 没有诚实的归属);③sink 存活:netboot
  Options 增 `SyslogOnly`,`MAMMOTH_PXE_ENABLED=false` 的纯 vMedia 部署
  以 sink-only 形态独立起 514(external 模式既已无条件绑定,语义一致,
  不新增配置);④契约不动:日志仍走 slog→TaskLogTee→task_logs,左树右流
  无需新 API(轮询 cursor 近实时,logs SSE 属后续可选)。测试:IPRegistry
  单测 + sink 归因回退/Forget + SyslogOnly 冒烟 + 三方言渲染断言。

## 装机网段地址服务可观测与边缘地址池(✅ 落地 2026-11-07;边缘实时门控设计保留备用)

**背景**(2026-10-30 现场系列实测):装机网段的 DHCP 服务健康度
是 PXE/vMedia 装机成败的高频根因,且当前完全不可观测——站点 DHCP 抖动、
无站点 DHCP、池耗尽三种形态的失败都表现为装机静默卡死或装好系统离线,
排障成本极高。设计讨论定稿如下。

**✅ 实现注记(2026-11-07,切片 1:引擎内建观测 + edge 饥饿上报 + 提交预检)**:

1. **观测核心**(`internal/netboot/observe.go`,Observer):per-MAC DHCP
   状态机——DISCOVER≥3 次/3 分钟窗口无地址后续 → 根因行("no DHCP
   answered on this segment";proxy 形态=网段无地址服务,pool 形态=站点
   侧静默);地址 REQUEST(option 50/ciaddr/bound-ip)= 健康对照信号清态;
   归因链复用引导条目注册表的 TaskID(武装 MAC 才有归属,外来客户端
   纯引擎日志);抑制窗 5 分钟防刷屏;任务日志经 TaskLogTee 落 task_logs;
2. **装机后离线观测**(installed-orphan):reaper 孤儿清扫删条目前先向
   Observer 注记 MAC→终态任务;此后该 MAC 的 DISCOVER = "刚装的机器无
   地址服务"警告,归属到已 succeeded 任务日志(免费装机质量回访);
3. **edge 饥饿上报**:`POST /netboot/sightings` 增 `starved` 标记(edge
   侧 per-MAC 计数 ≥N 次无地址 REQUEST 即置位)——pending.sighted 事件
   携带 starved,引擎侧告警;
4. **提交预检**:零声明网络的 install 提交时,按机器硬件 MAC 查近期
   starved sighting → 非阻塞警告行(202 照常;声明静态网络的规格豁免
   ——警告的受众就是零声明形态);
5. **净boot 无库依赖**:Observer 不碰数据库——归因走回调链(默认 =
   注册表),reaper 交接经内存注记;Tests:五态单测(阈值/对照清态/
   外来无归属/orphan 武装/窗口重置)+ PG 预检(警告落行/声明豁免/
   非阻塞)。
   **边缘实时门控**(定稿后半:实时引擎查询 + fail-closed + TTL 负缓存)
   设计保留备用——触发条件:edge 跨网段部署出现"未登记 MAC 白吃地址"
   的真实问题;当前 edge 的 dnsmasq allowlist 已覆盖第一层。

1. **引擎内建模式(单网段最简部署,引擎即在 L2)——主检测点**:
   - 池模式:租约发放/拒绝/耗尽事件归属已登记任务(现仅引擎日志一行);
     **装机后离线观测**:已完成任务的 MAC 持续 DISCOVER → 在已 succeeded
     的任务日志补 WARNING("刚装的机器无地址服务")——免费装机质量回访;
   - 内建无池(proxy 语义):install_os 期间 per-MAC 反复 DISCOVER 而无
     地址后续(无选择站点服务器的 option-50 REQUEST)→ 任务日志根因行
     ("网段无 DHCP 应答"),把 90 分钟超时才发现的排障压缩到第 3 分钟。
2. **edge(external 多 VLAN)**:per-MAC DISCOVER/地址 REQUEST 状态机
   (复用 handle() 现有解析),DISCOVER≥N 次无地址 REQUEST → 饥饿
   sighting 上报引擎;`REQUEST 选择站点服务器并携带 option 50` = 健康
   对照信号。
3. **提交时预检**:引擎按网段聚合近期饥饿报告;未声明网络的 PXE 任务提交
   时附非阻塞警告(概率性提示,运行时检测为主力)。

**边缘地址服务实时门控(设计讨论定稿,替代"定时同步允许列表")**:
edge 架构下引擎本就是引导链的硬依赖(trampoline 实时按 MAC 拉配置),
对"是否发租约"加实时引擎查询不引入新的可用性依赖——比定时同步更优(无
陈旧窗口)。设计要点:查询预算 1-2s(DHCP 客户端 4s/8s 重试容忍);
引擎超时 fail-closed + 短 TTL 负缓存(防外部客户端重试风暴);判定武装后
正缓存覆盖装机期;续租按本地已授租约表应答(引擎中途宕机不影响在装任务)。
可选端点:复用 script 端点作 oracle,或专用 GET /netboot/armed/{mac}。

**装机网段无 DHCP 服务时的硬性要求**:装好系统的网络必须**规格声明静态
网络**(spec.Network)——池地址是引导期临时身份(15 分钟内存租约,不持久、
不保证复用),不得渲染进目标 netplan;池的 DHCP 应答按设计只服务有登记
任务的机器,装好的系统拿不到池内地址。零声明规格在有站点 DHCP 的网段
由 DHCP 兜底 netplan 覆盖(cb72068)。

**已修复的相关项**:本地源摘要探测回退(a97671a,迁移自愈)、未声明网络
全 NIC DHCP 兜底 netplan(cb72068)、完成回调 3×10s 重试(d4dbdae)、
INSTALL_CONSOLE=vga(3e21237)。

**实测补充(2026-10-30,PoolURL 默认保留)**:netboot seed 的 target apt
primary 指向引擎 PoolURL(ISO 解包树)已实测完成全绿装机。注意事项:ISO
固有 apt 仓形态只有 dists/<suite>/Release + Release.gpg、**无 InRelease**,
subiquity 的镜像探测在**安装器环境无网/探测失败**时会回落交互确认门
("Continue with autoinstall?"——曾与池段耗尽叠加误判为 PoolURL 问题,实
为安装器环境网络所致)。企业内网建议 MAMMOTH_UBUNTU_APT_MIRROR 指向带
签名的本地镜像(aptly/reprepro,自带 InRelease);若将来要让引擎把 pool
树升格为一等 apt 源,需生成带签名的 apt 元数据(apt-ftparchive + 签名
密钥管理),设计后再议。

## 镜像直写通路(设计定稿,2026-11-06,待实施)

> 镜像派(dd 直写)立项:业界主线(Ironic `direct` 默认 / Tinkerbell 流式 /
> MAAS curtin 解包)全行业绕开安装器内联跑(related-work §1 同结论);
> mammoth 的 agent initramfs 通路天然能 dd(related-work §6 预留的 v2 接入点,
> 契约骨架不改),工控机 POC 评估 §八 已定案 dd 运行时选型。

**定案(吸收 thor-pxe-demo 真机/qemu 实录 + 评估文档 §八)**:

1. **dd 模式优于 url= 模式,产品化只做 dd**:url= casper 需把 live ISO
   整体拉进 RAM(3.6G+,限大内存机,qemu slirp 实测拉不动 3.6G;真机也需
   5-10 分钟才到 dd 点);alpine netboot 秒级到达 dd,~256-512MB 内存即可,
   面对工控机/边缘设备全集不假设大内存。url= 仅保留在 thor-pxe-demo 作
   Thor 类大内存机器的技术路线验证。
2. **解压契约(busybox 无 xz,POC 实录的硬约束)**:`image.format`
   三档,按依赖递增:
   - `raw`(未压缩)——零依赖,Canonical/img 直写退路介质形态;
   - `raw.gz`——**busybox gunzip 内建,推荐交付格式**,零依赖;
   - `raw.xz`——需 `apk add xz`(agent 通路 alpine_repo/apks 由引擎池携带)
     或 apkovl 预置 xz 二进制——可选增强,不进第一切片。
   POC 实录佐证:demo 的 `xz -dc | dd of=nvme bs=4M conv=fsync` 管道在
   casper live(ubuntu 带 xz)验证成立;alpine 侧同管道需先解决 xz 在位。
   进度上报不用 dd status=progress(busybox 版本差异),按 (bytes/总量) 自算。
3. **契约形态**(related-work §6 预留:agent 通路天然承载,不动契约骨架):
   - `image.kind: "dd"`(缺省 iso = 现行方言通路)+ `image.format` 三档;
   - `storage` 退化为**单目标盘选择器**(镜像自带 GPT/ESP,引擎不再声明
     分区;`storage.lvm`/raid 与 dd 互斥,提交即拒);
   - **firstboot = cloud-init NoCloud seed**:引擎把 spec 的
     identity/network/access 现行意图块渲染成 seed 写入(独立小分区),
     thor-pxe-demo 的 user-data runcmd 形态即已验证的注入模式——这是引擎
     对裸 dd 的增量价值(网络/凭证意图不丢)。
4. **运行时复用面**(= docs/12 agent-initramfs 第三使用者,换 payload):
   netboot 树 + apkovl + plan 变量 + bail 哨兵 + syslog 归因(2026-11-06
   刚修好的 script 抓取注册链)+ HTTP Range 断点续传(大镜像断点重拉)+
   NIST 800-88 清洗前置(已有 DriveEraser)+ **读回哈希验证**(conv=fsync
   写入 + 从盘读回逐字节比对——流哈希看不见盘上真字节,LSI write-back
   cache 教训;IMAGE_BYTES 由镜像库 resolution 携带)。**强依赖:机器侧
   工具池 = alpine-extended ISO**(MAMMOTH_WINDOWS_APPLY_ALPINE_ISO;seed
   注入的 dosfstools、raw.xz 的 xz 都出自它;standard ISO 的 /apks 为空,
   见 operations §4.5 第 10 项)。
5. **镜像封装规范(CIDATA 槽契约,2026-11-06 2288H 真机定案)**:dd 镜像
   须自带分区表与引导器,且**最后一个分区为 ≥16MiB 的 CIDATA 槽**(GPT
   type=0700 即可,内容不重要——agent 运行时 mkfs.vfat 覆盖后写入 NoCloud
   seed 三件)。机器侧**不做任何分区表操作**:alpine 的 sfdisk 包对大
   MegaRAID 卷 append 确定性 EINVAL、且 apk 包版连自建 GPT 都读不出
   (ab-v22..26 六轮实证),内核自解析 GPT(partx -a 强扫)+ /proc/partitions
   取槽是唯一可靠路径。示例构建(parted,任意宿主):
   `parted -s img.img mklabel gpt mkpart system 1MiB 31MiB mkpart CIDATA
   31MiB 47MiB`。无槽镜像 + seed 意图 = 任务失败(错误点名契约)。
6. **切片**(qemu 全程可验,不占真机窗口):
   ①~~agent dd payload + `image.kind/format` 契约 + 写后校验(raw 起步)~~
   **✅ 代码落地(2026-11-06,f41e2b4 后续)**——契约定案微调:通路开关=
   `image.distro: "dd"`(驱动注册表分发,免新增 kind 字段)+ `image.format`
   (raw|raw.gz,openapi 已收);单遍流 `wget→gunzip→tee(盘)→sha256sum`
   替代两遍方案(tee 直写 + 流式哈希 = 单趟全量校验,pipefail 兜上游);
   prepare 跳过 ISO 解析(distro=dd 无介质,机器直连源 URL)+ 无 apks 子树
   (busybox-only)+ verify_ready 家族短路(完成报告=验证面,同 windows);
   ~~qemu 冒烟待窗口~~ **✅ 容器冒烟通过(2026-11-06,dd_smoke_test.go,
   MAMMOTH_DD_SMOKE=1)**——dd_image 真实运行(wget→gunzip→tee→sha256sum
   对真实 HTTP 源),写盘字节哈希断言通过;seed 分区手术不进容器冒烟
   (alpine sfdisk 对普通文件确定性 EINVAL,loop 节点分配是 Docker 工件),
   归 qemu 全链轮(真内核/真块设备,语义与生产一致);
   **✅ 2288H 真机全链打通(2026-11-06,六轮迭代)**——镜像库引用→file
   端点拉流→写入→**读回哈希验证**→CIDATA seed 注入→applied 回调,全绿。
   真机收敛五项修复:plan 哈希剥 sha256: 前缀、busybox lsblk 无 -b/-o
   (改 /proc/partitions 整盘名正则)、控制器名 size 解析内核设备、写入
   conv=fsync + blockdev --flushbufs(LSI write-back 下紧随读拿到旧盘
   内容)、seed 弃 sfdisk(alpine apk 包不识 GPT)改 CIDATA 槽契约
   (镜像最后一个分区即槽,内核自解析 + partx -a + mkfs 覆盖);
   ②~~NoCloud seed 注入 + verify_ready 对齐~~ **✅ 代码落地(2026-11-06,
   0c331cd 后续)**——identity/network/access 意图随 CIDATA 分区生效:运行时
   写后追加 16MiB vfat(sfdisk --append,扩展池 apk 工具),seed 三件
   (meta-data/user-data/network-config)base64 内嵌 plan;网络 = netplan-v2
   平面接口(MAC 匹配,bond/vlan 诚实拒);口令 crypt SHA512(复用 autoinstall
   导出);prepare 在 seed 意图时才挂扩展池(alpine_repo 条件进 boot 行);
   verify_ready 维持完成报告=验证面(镜像首启是镜像自己的事);
   ③~~raw.xz(apks 携带)~~ **✅ 代码落地(2026-11-06)**——format 三档
   收齐(raw/raw.gz/raw.xz;raw.gz 为推荐交付,raw 未压缩是零依赖底座);
   raw.xz 在运行时 apk add xz(boot 行 alpine_repo= 条件同 seed,prepare
   按 format/seed 双动因挂扩展池)。**签名校验 = 镜像工件面硬化项,不入 dd
   通路**——dd 的写盘流校验已覆盖完整性(流上 sha256 对比 spec 声明,API
   信任模型承载意图真实性);发布者真实性(谁构建的镜像)属镜像库注册期
   验证(引擎持文件可跑 minisign,纯 Go ed25519 可实现),随 imaging
   工作流立项,见「长期方向」。
   **双载体补全(2026-11-06 真机后修订)**:vMedia 载体落地——alpine
   standard ISO 挂 BMC 引导 agent(引导离线,plan/overlay 在介质根),
   工具池 extended ISO /apks 提取到任务 netboot 路径迷你池(boot 行
   alpine_repo 指向);PXE-only 边界解除,覆盖矩阵 = {有/无带外} ×
   {有/无 PXE} 中除「无带外×vMedia」(语义矛盾)外全通。
   **~~重试误路径~~ ✅ 已修(2026-11-06)**——根因:install_os 的 applied
   分支是 windows agent apply 专属的 boot-two 状态机,却不分通路;dd 的
   applied 同样落进去,wimboot 重建拿整盘镜像跑 7z 提取必炸 UDF 报错,
   重试轮重进同一分支直至尝试上限。定案修复:dd 的 applied 即终态
   (切片②「完成报告=验证面」的落地)——install_os 按 prepare 持久化的
   plan 内容判通路(MAMMOTH_DD_MODE,老任务同样命中),dd 分支同步
   释放引导载荷(PXE 条目掉盘/vMedia 立即弹介质——机器已自重启,标准
   3 分钟 grace 是给还要读介质的安装器的,dd 没有)+ 记录终态
   completion(status ok,applied_detail 随行);dd 驱动补
   InstallerAutoReboot(agent 报告后自重启的事实,完成尾不再 cycle 正在
   首启的机器);windows apply 的 boot-two 原路径不动。**遗留**:
   真实可引导镜像端到端待镜像到场。

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

## 版本策略

- M3 为 v0.1(第一个可用版本);M4 为 v0.2;M5 为 v0.3;M6 为 v1.0
- **v1.0(2026-09-18 定案:就地落地)**:门槛四项——契约冻结(仅新增演进)、
  双发行版端到端、备份恢复文档、安全基线审查——2026-09 前全部达成;落地时
  的实际交付已远超门槛:三方言四镜像真机全链(虚拟介质 + PXE 双载体)、
  shim+grubnet Secure Boot、零注册入门、装机通路工程收尾(syslog sink /
  共享池缓存 / OACK 容忍 / apt 信任收尾)。曾一度把 tag 延后至"PXE 增强 +
  BiosSetter 完成",2026-09-18 复议取消(前提消失、动机被现有路径覆盖),
  边界外移项见"下一阶段"。
- **v1.1.0 已发布(2026-09-19)**:agent initramfs ✅(docs/12;qemu 双固件 +
  **真机闭环**,含控制器卷盘名解析)+
  BMC 能力接口三部曲(FirmwareInventory ✅ → BiosSetter ✅ 真机写回归 →
  NIST 800-88 擦盘 ✅ 真机结论,两段式确认契约随行)+
  PXE(外部 DHCP+TFTP 逃生门 ✅、arm64 链路 ✅ qemu 可验)+
  uniontechos ✅(根因定位修复,真机双通路闭环)+
  驱动加固(会话复用、iBMC 6.41 写形态、NormalizeESP)。
  剩余随 v1.x 增量:UefiHttp/Windows(用户脚本前后置接入,见下一阶段 5)/
  复验轮(等真机窗口)、UOS 最新版复验
  (等 ISO);契约仅新增演进(向后兼容字段/端点),破坏性变更进 v2 讨论。
- 发布流程:`git tag vX.Y.Z && goreleaser release --clean`(amd64/arm64,
  版本与 commit 经 ldflags 注入)。
