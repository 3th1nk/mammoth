# 12 · Agent Initramfs 安装路径(试点)

> 状态:试点已落地并通过 qemu 全链验证(2026-09-18,BIOS + UEFI 双固件)。
> 本文是该试点的结论记录:动机、架构、实现边界、验证证据与后续判断。
> 决策出处:docs/09-roadmap.md "下一阶段"第 1 项;docs/related-work.md §1
> 修正结论(行业绕开"安装器内联跑")与 §3 行动清单 ③。

## 1. 动机与问题定义

PXE 三方言真机闭环(debian13 十一项连环修、ubuntu22/24 十八项)暴露的
11+18 个发行版安装器缺陷,几乎全部属于同一 bug 类:**方言安装器的假设
摩擦**——netcfg 晚到、apt-setup 验签形态、by-hash 货栈、update-grub 在
chroot 里暴毙、subiquity 的 identity 校验时序、crypt 密码渲染……竞品调研
(related-work §1)给出行业答案:MAAS/Ironic/Tinkerbell 全部绕开"发行版
安装器在目标机上跑",改为**临时系统 + 自有运行时**做镜像解包/配置。

mammoth 的价值主张是声明式 Install Spec(存储三档/network v2/身份/脚本),
发行版方言只是把 Spec 翻译给安装器的渲染层。若存在一条**不经过安装器、
直接消费 Spec** 的安装路径,则:

- 新发行版接入成本从"写一个方言驱动 + 真机联调安装器"降为
  "包仓库可达 + 一个签名声明";
- 安装期行为 100% 可控(所有错误出自 mammoth 代码,诊断面收敛);
- 方言路径保留为兼容模式(kickstart/preseed 服务的存量发行版不受影响)。

试点目标(roadmap 原文):自有最小 agent(分区 + 从池装内核/包),六阶段
流水线与声明式 spec 原样承载,**qemu 全程可验、不碰真机**,结论决定去留
与所需 API 形态。

## 2. 架构

### 2.1 总览

```
提交(alpine spec)──→ 六阶段流水线(不变)
                        verify_layout ──→ configure_raid ──→ prepare_media
                                                               │ 渲染 agent plan(JSON + sh)
                                                               │ builder 造载体(apkovl overlay)
                        boot ←── boot-<token>.iso / PXE 引导树 ─┘
                          │
                 ┌────────▼─────────┐
                 │  alpine 内存系统  │  ← 与 ramdisk 探针同载体(复用全部坑位结论)
                 │  mammoth-agent   │
                 └────────┬─────────┘
        读 plan → 分区/格式化/挂载 → 从池装 alpine-base+kernel+openssh
        → 配置(身份/网络/shadow/钥匙/runlevels)→ grub → 回调 → 重启
```

### 2.2 组件落点

| 组件 | 位置 | 说明 |
|------|------|------|
| 驱动 | `internal/render/agent` | `agent.New("alpine")`,OSDriver 全量实现 + PXEDriver(Full)+ NetbootInstallCarrier(alpine_netboot)+ AgentInstaller 能力 |
| plan 渲染 | 同上 `renderJSON`/`renderSH` | 一份数据两种编码:`agent-plan.json`(契约面,可文档化/可被未来 agent 实现)+ `agent-plan.sh`(执行面,busybox 无 JSON 解析器;引号全部活在 shell 里,preseed 驱动同理由) |
| agent 运行时 | `internal/builder/agentboot.go` | `agentScript()` busybox sh;`AgentOverlay()` 打包 apkovl;`BuildAgentNetboot()` 出 PXE 引导树 |
| 载体接线 | `provision.strategy_pxe.go`(alpine_netboot case)/ `bootSession.Seed`(虚拟介质 seed 注入) | overlay 不走 answers(二进制内容不进 JSON context) |
| 布局识别 | `builder.DetectLayout` alpine 分支 | alpine ISO 可走 `BuildBootISO` 通用全量重打包(此前只有探针直接调 rebuildPatchedISO) |
| 注册 | `cmd/mammoth/serve.go` | 一行;capabilities 自动出现 alpine(keep=none / pxe=full) |

### 2.3 agent 职责边界(试点版)

做:声明式分区(GPT/dos 表按固件自动选;UEFI 且 boot 盘未声明 ESP 时自动
补 300MiB ESP)、mkfs(ext4/vfat/swap)、挂载、从池装 `alpine-base
linux-lts openssh`、身份(hostname/shadow crypt 密码/root 钥匙/sshd)、
静态或 DHCP 网络(ifupdown-ng 形态)、openrc runlevels、grub(按固件
grub-bios/grub-efi;`--root-directory` 免 chroot)、pre/post 用户脚本
(base64/url 两形态 + expected_exit 语义)、完成回调、自重启。

不做(渲染期显式拒绝,不是静默缺失):RAID(软/硬)、bond/vlan、
keep 任何形态(SupportLevel=none,提交门禁即拒)、xfs 等 ext4/vfat/swap
之外的文件系统、preserve。

### 2.4 关键设计决定

1. **plan 双编码**。JSON 是给人与未来实现看的契约;sh 是真执行面。
   把两者合成一个文件(如 JSON+heredoc)会牺牲 SOL 可读性;分开则
   渲染层完全对称,代价仅是两份产物的一致性(同一输入同源生成)。
2. **运行时脚本与 plan 解耦**。agent 脚本 plan 无关(只认 plan 的数据
   语法),因此 overlay 可以按载体构建一次,不随任务 render 变化;
   虚拟介质路径 overlay 是 ISO seed,PXE 路径 overlay 在引导树里,
   同一份内容两种投递。
3. **载体 = 探针载体**。alpine netboot tarball(kernel/initrd/modloop)
   + apkovl overlay,全部 2288H 真机踩过的坑(apkovl 靠 URL 拉取而非
   文件名参数、/apks 必须随载体、modloop 走 HTTP)直接继承。
4. **装完的机器 = 标准alpine**。agent 产出的不是精简镜像,而是从池装的
   正常系统(linux-lts + mkinitfs + grub + openssh + openrc),SSH 直通、
   apk 可继续装包——"从池装系统"的池就是发行版官方 ISO 的 /apks。
5. **方言降级兼容模式**。已有方言驱动一字未动;alpine 是第一个
   "无方言"成员,后续新发行版优先评估 agent 路径(见 §6)。

## 3. 试点过程发现(第一手结论)

按时间序,每条都是 agent 路径独有的坑位结论:

1. **alpine standard ISO 的 /apks 是 boot 池不是系统池**(101 个包,无
   linux-lts/grub)。"从池装系统"必须用 **extended ISO**(158+ 包,含
   内核/grub-bios/grub-efi/mkinitfs/dosfstools)。这与 debian netinst 池
   缺 netboot udeb(kernel-image-di)是同型发现:**镜像的随附池只覆盖
   它自己引导所需,不覆盖"用它装系统"所需**。签名声明化(roadmap 第 3
   项)需要给池能力建模(boot_pool vs system_pool)。
2. **DetectLayout 不识别 alpine**(探针 ISO 一直绕过通用 BuildBootISO
   直调 rebuildPatchedISO)。补齐后 alpine 成为一等布局家族——这也是
   distro 签名声明化的方向验证:布局知识应该数据化。
3. **openrc 的 local 服务用 eval 在服务 shell 里执行 .start 脚本,该
   shell 带 errexit**——任何一条命令失败都会静默杀死整个运行时(无
   FATAL、无回调、直接回 login)。探针从未踩中是因为它的每条命令都自带
   容错。agent 的修复:进脚本先剥 `-e`(自管错误),错误路径统一走
   `bail`(回调 + 落 shell,SOL 可诊断)。教训:**ramdisk 运行时必须
   假设宿主 init 系统会注入意外 shell 语义**。
4. **sfdisk 的分区名解析**:`/dev/nvme0n1` + `1` 直接拼接得到
   `nvme0n11`(被解析成 11 号逻辑分区)→ "Extended partition does not
   exists"。nvme/mmcblk 的 pN 规则必须在**写表时**就正确(sfdisk 按
   输入行里的名字原样解析),读侧惯例(nvme0n1p3)同样适用。
5. **全新空文件系统上没有 /etc**。挂上刚 mkfs 的根就要写 fstab/拷钥匙,
   而 alpine-baselayout 的目录树要等 apk 装完 alpine-base 才出现。挂载
   后立刻补最小骨架(etc/root/tmp/dev/proc/sys/media/var/boot)。
6. **chroot 内 apk 够不到本地 apks 池**:bind mount 不递归(/media 的
   子挂载 cdrom/apks 不会跟着进 chroot),chroot 里 apk 报
   "WARNING: opening /media/cdrom/apks: No such file or directory →
   grub (no such package)"。解法:**不 chroot**——grub-install
   `--root-directory=$TARGET` 从 agent 环境直接对 target 装,设备探测
   用 agent 环境的活 /dev /proc /sys。curtin"装完再配置"的时序优势
   (related-work §3)在本路径下的等价物:能不进 chroot 就不进。
7. **环境噪声即联调成本**(过程实录,非 agent 缺陷):7 月遗留的
   `go run` 僵尸 runner 进程抢任务(SCHEMA_UNKNOWN_DISTRO 三轮误诊)、
   248 旧 mammoth 经 docker 端口映射反连本机 dev PG 抢队列(换独立
   DB 解决)、并发 qemu 共写同一 serial/disk 文件制造"死亡现场"。
   HANDOFF "防本地 runner 抢任务"的告诫值得固化为 harness 约束:
   e2e 必须独立 DSN。

## 4. 验证证据

`scripts/agent-dev/run.sh`(自建 mammoth + 独立 DB agent_e2e + qemu):

- **BIOS**:提交(extended ISO,ESP+swap+root rest,静态无、DHCP)→
  prepare_media 6s(渲染 plan + 全量重打包)→ qemu 引导 → agent 全链
  (分区 dos 表 → 装池 158 包 → 配置 → grub MBR → 回调 ok → 自重启)
  → **六阶段全绿** → 重启后 SSH(声明钥匙)命中,`hostname` =
  `agent-bios-1`(HostnamePattern 展开正确)。
- **UEFI**:同链,OVMF 引导(GPT 表 + grub-efi ESP fallback:
  `--removable` → EFI/BOOT/BOOTX64.EFI);重启后 SSH 命中
  `agent-uefi-1`。复跑:`AGENT_E2E_FIRMWARES="bios uefi"
  ./scripts/agent-dev/run.sh`(前置:alpine extended ISO 于
  ~/mammoth-qxe/,独立 DB agent_e2e,见 scripts/agent-dev/README)。
- 失败路径验证(nvme 设备名 bug 轮):agent 报 FATAL → POST failed →
  流水线 INSTALL_FAILED 可重试;SOL 落 shell 可诊断。
- 单测:`internal/render/agent`(plan 渲染/网络/脚本/校验/能力)、
  `internal/builder`(overlay 结构);全仓 `go test ./...` 绿。

## 5. 与现有架构的贴合度(试点验收判据)

- 六阶段流水线零改动承载(无新 stage、无新 context 字段,除
  bootSession.Seed 一处载体注入);
- 声明式 spec 原样消费(同一 validateInstallSpec 入口;keep 门禁经
  SupportLevel=none 自动生效;固件门禁/支持矩阵自动覆盖);
- 双载体自动可用(虚拟介质 + PXE 均为现有机制);
- 零注册/观测/寻址等基础设施全部无感复用(agent 就是个普通 distro)。

## 6. 结论与后续

**结论:方向成立。** 六阶段 + 声明式 spec 的架构红利兑现:一条全新安装
路径的接入只花了渲染驱动 + 载体 case + 一行注册;所有摩擦都在 mammoth
自持代码里,诊断面完全收敛。

**边界(试点明确不做,按需再开)**:
- RAID/bond/vlan/keep/xfs(渲染期显式拒绝,见 §2.3);
- arm64(载体 tarball 换 aarch64 即可,等真机窗口);
- Secure Boot 引导链(agent 的 grub 是 alpine 签名,不在 shim 信任集;
  SB 机器继续走方言路径或后续评估);
- 真机回归(2288H 轮装时顺带:UEFI x64 一轮即可,载体与探针同源)。

**对 roadmap 第 3 项(发行版接入声明化)的直接输入**
(评估结论:注册面声明化已收敛——agent 三件套留在 agent 驱动内声明,
决策记录见 docs/06 §6):
- 池能力必须建模:boot_pool(引导自足)vs system_pool(可装系统)——
  extended/standard 的差异就是这个字段;
- DetectLayout 的布局知识(alpine 分支)是签名表的第一个候选条目;
- agent 路径的新发行版接入清单 = {extended 化的 ISO 或外部池、
  包集声明(linux-lts 等价物)、引导包名(grub-bios/efi 差异)}——
  全部可 JSON 化。

**对 API 形态的影响**:无契约变更(distro 自由字符串 + 能力接口门禁
足够)。未来若 agent 路径转正,值得考虑的仅是:plan 的 JSON 面可挂到
install-plan 端点作 dry-run 产物(dialect: "agent" 的渲染预览)。

## 真机闭环(2026-09-19,2288H V5 / LSI LogicalDrive0,✅)

六阶段全绿零人工:LSI 3.6T 卷装机 + 无人值守自举 + SSH 钥匙直通
(agent-2288h)。真机暴露并当日修复:

1. **控制器卷盘名解析缺位**(0bede94):plan 把 Redfish 名(LogicalDrive0)
   直接当内核设备名,sfdisk 报 cannot open——kickstart %pre 与 autoinstall
   resolve-disk 均有 size±1% 解析,agent 路径补齐(plan 携带 size_bytes,
   runtime resolve_disks 解析并同步重写分区键);内核名直通不解析;
2. **诊断链固化**(迭代九轮的真金):storage/sfdisk 全去静默进 err 文件 +
   sfdisk 瞬态重试、log 双写文件、bail 上报携带 script 版本 + err/log
   轨迹——控制台/KVM 不可靠时(慢 init 或断连)回调详情是唯一信道;
3. 排障过程的教训:**spec 缺 network 字段是 agent 装机静默挂死的形态**
   (回调不可达),基线注记见 runbooks/test-baselines.md;**部署链的
   pkill 自匹配陷阱**会导致"以为部署了其实服务还是旧二进制"(ISO 内容
   与二进制版本核对是排障第一课)。
