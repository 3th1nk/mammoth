# Runbook · 2288H Windows 真机窗口(windows2019 agent apply-image 通路)

> **状态(2026-09-22 方案 A 真机闭环全绿,本通路收官)**
>
> **六阶段全绿(job_57ef0347654e / tsk_705652016c55,2288H 真机)**:
> agent 段(PXE→alpine agent→拉 4195MiB wim→wimlib 直写→in-wim 注入,
> 12:18-12:24)→ applied → 二段武装(断电→同 token 重建 wimboot bcdboot
> 树→条目翻转→PXE once 上电,12:28)→ bcdboot 段(plain iPXE→wimboot
> WinPE→bcdboot 生成 BCD→**bcdedit 枚举成功(NT BcdOpenStore 门禁通过,
> 0xC0000098 死点终结)**,12:32)→ 释放 PXE+power cycle → first boot
> (specialize 无错误框)→ 12:39 完成回调 "windows setup finished" ok →
> verify_ready ✓。静态网 198.51.100.215 按 spec 落网(248 ARP 表证实,
> MAC <LOM-MAC>;ICMP 被防火墙拦属预期)。全程 ~28 分钟。
>
> **二段式时序实录**(方案 A 设计→真机零偏差):applied(12:24:50)→
> armed(12:28:35,断电+建树 ~3.5min)→ 上电(12:28:39)→ wimboot 脚本
> 下发(12:31:16)→ 裁定标记 BCDBOOT OK(12:32:34)→ PXE 释放(12:32:36)
> → cycle(12:32:39)→ 回调(12:39:27)。startnet 的 curl POST 标记 +
> bcdedit 门禁(BCDBOOT OK 要求 bcdboot 与 bcdedit 双零退出)按设计工作;
> WinPE 内 curl 由 builder 从 install.wim 提取补种(c3d99ca)。
>
> **已知行为(非缺陷,记录)**:完成回调源 IP = 198.51.100.218(站点路由器
> DHCP 租约与声明静态并存,Windows 默认路由竞争选了 DHCP 地址)——spec
> 落网判定看 ARP(.215→目标 MAC ✓),引擎记录的 ssh_address 会是租约地址;
> 需要回调源=声明地址的话得压 metric 或关站点 DHCP(未做)。
>
> **装完控制台与远程管理(注入器 v7)**:**挂死根因(UnattendGC 全量
> 日志实锤)= agent 通路的 SetupComplete 真会执行**(windeploy
> RunUserProvidedScript;setup.exe 流程反而不执行)——ps1 作为 SYSTEM
> 在 winlogon 执行 AutoLogon 前删掉了 AutoAdminLogon/DefaultPassword,
> 自动登录被打断 → 挂死在 msoobe 控制台框;而 SetupComplete 的 35 秒
> 窗口里回调就已发出,所以"六阶段全绿"与"界面死框"长期并存。v6:清理
> 动作加 `IsSystem` 守卫,只跑在 FirstLogon 用户上下文,logoff 移除。
> v7:**access.capabilities**(按任务 opt-in,缺省全关)——
> `["rdp","winrm","ping"]` 开对应防火墙组/远程桌面/PSRemoting/回显,
> 通道经 task.json 下机,未知值渲染即拒;操作面从不可靠的 iBMC KVM
> 转向网络(RDP / Enter-PSSession,凭据=Administrator/一次性密码)。
> 静态网顺带修复:New-NetIPAddress 前先 Remove-NetRoute 清 DHCP 默认
> 路由(网关重复使整条命令静默失败,.215 此前从未真正落地,回调一直
> 走租约地址)。msoobe "Failed to create the wizard 0x80040154" 为
> Server Core 正常形态("Continuing to mandatory tasks anyway"),与
> 死框无关。预备树缓存由 windowsInjectorVersion 护栏自动重建。
>
> **开放问题:装完系统文字渲染全坏(经典 GDI TextOut 失败)**——agent 通路
> 装出的机器,控制台/RDP 里所有文字(提示符、菜单标签)不渲染,输入无
> 回显;但窗口框架/颜色/图标正常,GDI+ 自绘文字正常(WinRM 实测位图渲染
> 完好),字体文件 279 个与 wim 哈希一致、注册表 103 项完好、
> AddFontResource 加载成功但 TextOut 仍返回 False——指向 win32k/GDI
> 字体表初始化失败,与 wimlib --no-acls 直写卷的某种系统级差异相关
> (setup.exe 装出的同内容镜像文字正常)。机器功能本体不受影响
> (回调/网络/WinRM 全通),管理走 WinRM(`winrs -r:http://198.51.100.215:5985
> -u:Administrator -p:<密码> <命令>`,或 248 上已装 pywinrm)。
> 待办:setup 装机对照取证(hive/服务清单 diff)、win32k 字体表初始化
> 机制调查、评估 wimlib apply 去 --no-acls 或补 acl 的影响。
>
> **历史卡点(已终结)**:首版"手工补丁 store"在 specialize 弹"无法更新
> 计算机的启动配置"——NT 的 BCD 层拒绝手工 hive(0xC0000098)。结论:
> 手工 hive 对 BcdOpenStore 永远不合法,修复 = bcdboot 原生生成
> (setup.exe 同时序),即两段式方案 A。
>
> **环境与访问**:
> - 248 = 198.51.100.248(SSH root 免密,<部署域名>);mammoth 服务
>   `/usr/local/bin/mammoth serve --mode=all --env-file=/root/mammoth.env`;
>   部署法:mac 交叉编译 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build
>   ./cmd/mammoth` → scp 到 248:/root/mammoth.new → kill 旧进程 → mv 到
>   /usr/local/bin/mammoth → setsid nohup 重启(注意 /root/deploy.sh 里的
>   env 路径是旧的 /tmp/mammoth.env,实际用 /root/mammoth.env);
>   **248 现行二进制 = HEAD(89bd602,含 auto 判定+capabilities+v10 网关幂等),与 HEAD 同步**;
> - **248 跑 builtin netboot 栈**(mammoth 自持 UDP 67/69/4011,
>   `MAMMOTH_PXE_ENABLED=true`),proxyDHCP 按 entry kind 自动发 NBP
>   (agent 条目→shim 链,wimboot 条目→plain ipxe-amd64.efi),二段切换
>   全自动零配置;external dnsmasq 仅是别处的逃生门;
> - alpine extended ISO 在 248:/data/os_iso/alpine/,env 已配
>   `MAMMOTH_WINDOWS_APPLY_ALPINE_ISO`;alpine netboot tarball env =
>   /data/os_iso/alpine/ 下的 3.22.2;
> - 机器 = mch_d5b1609a0290(2288H V5,iBMC 198.51.100.74 redfish 可达,
>   LOM Port1 MAC <LOM-MAC>);本轮装完 = win-agent-1,
>   Administrator 密码=<一次性口令>(事件 task.root_password
>   可查),静态 198.51.100.215/24 gw 198.51.100.1 + 站点 DHCP 租约 .218;
> - 凭据加密存 DB,MAMMOTH_MASTER_KEY 在 /root/mammoth.env;
>   iBMC 的 SOL 会话建立但零字节,排障不可用——用 diag 回传通道;
> - Windows ISO:/data/os_iso/windows2019/cn_windows_server_2019_x64_dvd_4de40f33.iso;
>   prepared 树缓存 pool-store/7582db01...(injector v10,内容变更自动重建;首次装机多一次 wim 手术,此后秒级命中)。
>
> **真机排障主通道**:VGA 在 initramfs 后冻结属预期(console=ttyS0 切换;
> first boot 期的黑屏+cmd 小窗口 = AutoLogon 收尾脚本在跑,正常);
> agent 步骤级 diag 回传(/render/<token>/diag/win-progress)+ bcdboot 段
> 裁定标记(diag/bcdboot-done.txt,首行 BCDBOOT OK|FAIL,附 bcdboot/
> bcdedit 日志)落盘 /data/mammoth/media/diag/<task>/;specialize 日志用
> alpine 诊断任务取回(modprobe ntfs3 → wget --post-file)。
> 248 上留了 /root/winev.sh(任务事件查询助手:winev.sh <task_id> [limit])。
>
> **boot.installer=auto 已落地(89bd602)**:提交时声明 auto → prepare
> 按部署事实判定通路——SMB 导出(MAMMOTH_WINDOWS_INSTALL_SMB_UNC)已
> 配置走 setup 主线,未配置走 agent apply;提交门对 auto 免 SMB 门禁。
> 缺省(不声明)仍是 setup。
>
> **已知小缺陷(2026-09-23 已修)**:~~cancel 不释放 netboot 条目~~——根因
> 是 runner/executor 的终态释放读 CLAIM 时任务快照,而 token/boot_strategy
> 是 prepare 阶段才 patch 进 context 的(陈旧解析误路由 virtual-media,
> 条目与 boot tree 全漏);终态释放改 `releaseBootPayloadFresh` 先重读
> DB 记录,PG 回归套件钉住(internal/provision/terminal_release_test.go)。
> ~~机器状态机装完停留 discovering~~——根因是 ramdisk 探针开了生命周期
> 转换不收口(装机流程不碰机器状态);探针成功与 verify_ready 成功两处
> 现在都回置 ready。~~同机多轮重试会堆积 pending 任务~~(已修 2026-09-24:
> 提交门 409 JOB_MACHINE_BUSY——install 任务针对同机存在
> pending/running/interrupted 前驱时整单拒绝,错误明细带阻塞任务/作业
> 标识;先显式 cancel 旧作业再重跑,PG 回归钉住)。
> 其余开放问题(文字渲染等)见上文 §开放问题。

> 目标(已收官 2026-09-22):agent apply-image 通路六阶段真机闭环验证
> ——多轮全绿,终态见顶部状态块;历史设计/实施细节见下文 §方案 A
> 实施清单与 docs/compat/distros.md §windows 落地节。

## §方案 A 实施清单(✅ 已落地并真机验证 2026-09-22;下文为实施记录)

外部评审(2026-09-22,专家意见 + PZ regf 详解 + ReactOS cmlib 源码核对)
定案:手工补丁的模板派生 store 对 NT 的 BcdOpenStore 永远不合法
(NT 校验项远多于 bootmgr;校验和/脏位均非根因),**唯一贴近微软原生的
修复 = 让 bcdboot 生成 BCD**——这正是 setup.exe 自己的时序。

**三段式流程**(同一任务,两次引导,✅ 已按此实现):

1. **agent 段**(✅ 已裁剪):alpine agent 引导 → 分区
   (GPT: ESP 300M + MSR 16M + NTFS rest,windows plan 现行 GUID)→
   mkntfs → HTTP 拉 install.wim → **in-wim 注入**(unattend-panther +
   task.json + SpBcd 剥离版 Specialize.xml)→ POST `applied` → 重启。
   **已删除**:BCD 预烤、ESP 手工落位、bcdpatch 模板改写、NVRAM 条目
   (bcdpatch_py.go/bcdgen.py 已删,wimlib/mkntfs overlay 不变;ESP 仍
   分区+vfat 格式化,bcdboot 的落点)。落点:`internal/builder/agentboot.go`
   win_apply、`internal/render/windows/windows.go` renderAgentApply。
2. **bcdboot 段**(✅ 已实现):编排侧在 `applied` 后**先断电**(机器正在
   自行重启,registry 行即将翻转,不按住会撞进半翻转状态)→ 同 token
   `BuildWindowsWimboot` 重建 boot tree(bcdboot 形态 startnet 烧入
   boot.wim;unattend/task seed 契约放宽为可选)→ netboot 行翻转
   wimboot 形态 → `SetBootDevice(PXE, once)` + power on(复用
   pxe().arm 尾部)。WinPE startnet(`bcdBootStartnetCmd`):
   ```
   wpeinit
   diskpart (ESP=p1 → S:)
   探 NTFS 卷盘符(扫 C/D/E/F/W 的 \Windows\System32)
   bcdboot W:\Windows /s S: /f UEFI
   bcdedit /store S:\EFI\Microsoft\Boot\BCD /enum all   ← 验收门禁
   curl POST 裁定标记(BCDBOOT OK|FAIL + 日志)→ diag 通道
   :hold 原地等待(绝不自启重启——自启会与 PXE 释放赛跑)
   ```
   编排侧等 diag 标记(`MediaDir/diag/<task>/bcdboot-done.txt`)→
   释放 PXE → power cycle → Windows first boot。state machine 持久化
   install.`bcdboot_armed_at`/`bcdboot_done_at`,stage 重入安全。
   落点:`internal/provision/install.go` installOSStage +
   armWindowsBcdbootStage/bcdbootMarkerState/bmcSetPower;
   `internal/builder/wimboot.go` seed 契约。
3. **first boot 段**(零改动):specialize(无 SpBcd,BCD 原生合规)→
   FirstLogon → 真实回调(`detail` 应为 "windows setup finished")→
   verify_ready。

**PXE 传送面(实现要点,清单外新增)**:两段共链。248 实跑 builtin
netboot 栈:proxyDHCP 按 entry kind 自动发 NBP(agent 条目→shim 链,
wimboot 条目→plain ipxe-amd64.efi),二段翻转全自动零配置。若换 external
dnsmasq 部署:agent 条目本来就用 iPXE 语法渲染(RenderScript 非 wimboot
分支),把机器 MAC 全程钉 plain iPXE(winboot pin)即可两段共链,无需
中途换钉。

**验收判据**(已内建为 startnet 门禁 + 常设校验):bcdboot 段的 WinPE 内
`bcdedit /store S:\EFI\Microsoft\Boot\BCD /enum all` 不报 0xC0000098
= store 对 NT 合法,裁定写入 diag 标记首行(OK 要求 bcdboot 与 bcdedit
双零退出)。

**窗口前置核查(实现未覆盖的运维项)**:①boot.wim 是否自带 curl.exe
(`7z l .../boot.wim` 查 `System32/curl.exe`;缺则从 install.wim 提取
补 seed——startnet 标记回传依赖它,setup 路径的 startnet 同样押注);
②external dnsmasq winboot pin(见状态块运维清单②)。

**方案 B(备选,若坚持 Linux 侧出 BCD)**:在任一 Windows/WinPE 环境
`bcdboot` 生成一份基准 store(一次性,人工取出),Linux 侧只用**就地
value 数据改写**(bcdpatch.py 机制,零分配)+ BCD 文件 4096 对齐。
该路径未真机验证,优先级低;bcdpatch/bcdgen 工具已从树中删除,git
历史可考。

## 0. 前置(全绿才开工)

- **248 服务**:含 `boot.installer` 契约的构建已部署(`GET /api/v1` 的
  `capabilities.windows_agent_installer: true`;若 false = 缺 alpine 配置)。
- **builder 外部依赖(248)**:`wimlib-imagex`(install.wim 预备树注入 +
  agent 侧 apply 都要;agent overlay 自带一份,服务端 wimlib 只在
  EnsureWindowsTree 注入时用)与 `7z`(UDF 解树)在位——wimboot 轮已核。
- **媒体三件**:
  - windows 2019 ISO(同 wimboot 轮,`/data/os_iso/windows2019/`);
  - **alpine extended ISO**(新增,`MAMMOTH_WINDOWS_APPLY_ALPINE_ISO` 指向,
    如 `/data/os_iso/alpine/alpine-extended-3.22.2-x86_64.iso` —— 机器侧
    包池:python3/sfdisk/partx/dosfstools;**standard ISO 不行**,无 python3);
  - alpine netboot tarball(`MAMMOTH_PROBE_ALPINE_NETBOOT`,探针轮已配)。
- **机器档案**:沿用 wimboot 轮(`mch_0f1e2d3c4b5a`,固件 uefi-x64 ✓
  UEFI-only 门禁;盘 LogicalDrive0 ≈3.64TiB)。layout 快照过期则 §1 discover
  必跑。
- **PXE**:external 逃生门与 wimboot 轮同(站点 dnsmasq 按 MAC 钉 shim
  链——**agent 模式发 shimx64.efi 不是 ipxe-amd64.efi**,dnsmasq 示例即
  kit 默认 conf;参考 scripts/windows-dev/external-win-e2e.sh 的 agent 分支)。

## 1. discover(若快照过期)

同 win2288h-runbook §1(ramdisk 探针);agent apply 依赖 layout 解析出的
盘设备,快照是 verify_layout 的输入。

## 2. 提交(installer=agent)

```jsonc
{ "boot": { "strategy": "pxe", "installer": "agent" },   // ← 与 wimboot 轮唯一差异
  "image": { "source": "file:///data/os_iso/windows2019/cn_windows_server_2019_x64_dvd_4de40f33.iso",
             "distro": "windows2019" },
  "storage": { "disks": [{ "select": {"match": {"type": "nvme", "size": "largest"}}, "wipe": true,
    "partitions": [
      {"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
      {"size": "rest", "fs": "ntfs", "mount": "/"}]}]},
  "network": [ /* 同 wimboot 轮:LOM MAC → 198.51.100.215 静态 */ ],
  "identity": {"hostname_pattern": "win-agent-{index}"} }
```

提交即验:无 `SCHEMA_WINDOWS_SMB_SHARE_REQUIRED`(agent 通路不消费 SMB);
错值(如 `imaging`/linux 发行版)应 `SCHEMA_INVALID_BOOT_INSTALLER`。

## 3. prepare_media 验证点

- 服务端日志:EnsureWindowsTree 复用 wimboot 轮的 sha 缓存(命中则秒级);
  `WindowsImageIndex` 解析出 SERVERSTANDEDCORE(2019 zh-CN = index 1)。
- `/netboot/script?mac=<LOM MAC>` 返回 **alpine 形态**(kernel vmlinuz +
  initrd.img + modloop/apkovl 参数)——不是 wimboot 形态。
- 冒烟(可选):`curl -sI http://<248>/netboot/store/<sha>/win/tree/sources/install.wim`
  应 200 且 Content-Length ≈ 4.3G;`.../efi/boot/bootx64.efi` 200。

## 4. boot→install_os 观察(机器面)

1. 机器 PXE → plain iPXE(或 shim→grubnet)→ alpine 起(mammoth-agent
   打头日志);
2. agent 拉计划 → 分区(wipefs+sfdisk GPT:ESP/MSR/NTFS)→ mkntfs →
   **wim 下载(4.3G,分钟级,RAM 预检失败会带分类 detail 直接 bail)** →
   in-wim 注入(unattend/task.json/Specialize)→ wimlib apply(进度走
   控制台+diag)→ POST applied → `reboot -f`。
3. 磁盘判定:`sfdisk -d` 应见 GPT + ESP/MSR/ntfs 三段。

## 5. first boot → 六阶段

boot-two 释放后直落盘:bootmgfw(ESP,bcdboot 生成)→ winload →
specialize/oobeSystem → AutoLogon + FirstLogonCommands → 静态网 →
完成回调(SetupComplete 与 FirstLogon 各一次,引擎取首条)→
install_os 释放 PXE 记录 → verify_ready(boot order 还原)。
验收:**六阶段全绿 + `.215` 按 spec 落网(ARP 层)+ 事件
`task.install_reported`**。失败面:bail 的 detail 带 err/log trail;
setup 路径保留为对照通路(同机提交不带 installer 字段即可复跑)。

## 6. 已知边界(预期内,非故障)

- wimlib 只铺文件:**inbox 驱动覆盖外的机型不在本通路射程**(无驱动注入);
- RAM < install.wim + 2G 的机器在 RAM 预检处 bail(2028H ≥128G 无虞);
- Secure Boot ON 需内核签名(当前 wimboot 与 agent 通路都要求 SB OFF);
- 装机期机器入站 ICMP/RDP 默认被 Windows 防火墙拦(同 wimboot 轮注记);
  `access.capabilities` 开启 rdp/winrm/ping 时,ps1 用 Any-profile 显式
  规则放行(Public 下默认无 RDP/ICMP 实例,zh-CN 的 netsh 组名不可用)。
