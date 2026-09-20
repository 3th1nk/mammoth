# Runbook · 2288H Windows 真机窗口(windows2019 虚拟介质全装闭环)

> **结果(2026-09-20 执行完毕)**:排障闭环,装机未启动——根因定案为
> **iBMC 6.41 虚拟 CD 对 windows 介质 UEFI 引导固件级拒绝**(原版/重打包
> ×NFS/客户端重定向 ×Once/Continuous 全灭,同通路 alpine/UOS 正常,完整
> 证据链见 compat/huawei.md windows 虚拟介质节)。管线侧当日另收获三修:
> install-plan 分类错误 500→422(eaf0ccd,已部署)、探针资产路径修正
> (WORKDIR 迁移遗留)、ssh.address→探针静态兜底真机实证。重跑前置:仅
> 等 workaround(USB / iBMC 升级 / Ventoy 式 builder 增强),§1 起流程不变
> (discover 快照有时效,重跑前先跑 §1)。

> 2026-09-20 定稿。目标:真固件 El Torito 引导 → unattend 全装 → SetupComplete
> 完成回调(真 mammoth CompleteURL 承接)→ LSI SAS3508 inbox 驱动验证。
> qemu(TCG)已收口应答受理层;本窗口是装机闭环与回调的最终验证面
> (docs/compat/distros.md §windows)。

## 0. 前置(2026-09-20 已全部核就)

- **248 服务**:`v1.1.0-win-dev`(eaf0ccd,ldflags 注入)已部署并运行,
  `GET /api/v1` 的 `distros[]` 含 `windows2019`(family: windows);
  `MAMMOTH_RAMDISK_ENABLED=true` 已补入 `/root/mammoth.env` 并重启生效。
- **builder 外部依赖(248)**:`wimlib-imagex` / `7z` / `xorriso` 三件全在位;
  今晨已用同一 cn_windows_server_2019 媒体跑通构建链(win-dev rig 产物
  `/data/mammoth/win-dev/boot-dev.iso`,4.8G)。
- **媒体**:`/data/os_iso/windows2019/cn_windows_server_2019_x64_dvd_4de40f33.iso`
  (4.8G)。iBMC 6.41 虚拟介质挂 4.8G ISO 预期可行(UOS 8.2G 已实证)。
- **机器档案**:`mch_0f1e2d3c4b5a`,state=ready;iBMC 198.51.100.74
  (cred_a9b8c7d6e5f4);固件观测 **uefi-x64**(windows2019 是 uefi_only,门禁会过);
  盘查 coverage=full,`LogicalDrive0` ≈ 3.64TiB(protocol raid)。
- **网络**:机房无站点 DHCP;LOM Port1 `02:00:00:D8:C6:97` 是唯一插线口;
  网关 198.51.100.1,DNS 198.51.100.3。目标静态地址 **198.51.100.215**
  (2026-09-20 扫段确认空闲;.211/.212 是本机历史安装残留,.213/.214/.219 被
  VMware 占用)。Windows 安装期零网络依赖(应答与回调全走本地介质+装后回网),
  SetupComplete 先配网再回调,顺序内建。
- **layout 快照已过期**(保留策略,LAYOUT_SNAPSHOT_REQUIRED)——**discover 是
  窗口必经第一步**,不能直接提交安装。

## 1. discover 重建快照(ramdisk 探针)

```bash
curl -s -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d '{"type":"discover","probe":"ramdisk"}' \
  http://198.51.100.248/api/v1/machines/mch_0f1e2d3c4b5a/actions
# 轮询任务态 + 完成后核对:
curl -s -H "Authorization: Bearer $T" \
  http://198.51.100.248/api/v1/machines/mch_0f1e2d3c4b5a/layout | python3 -m json.tool
```

预期:挂 alpine 探针 ISO → 引导 → /sys 上报 → 快照落库(source=ramdisk,
LSI 卷可见)→ 弹介质断电。本机三探针闭环已实证,预期零新发现。

## 2. install-plan 试算(只读预检)——2026-09-20 已预检通过 ✅

```bash
# 注意:install-plan 的 body 是裸 spec(不是 job 载荷;distro 必须在顶层)
curl -s -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d @/root/win2288h-plan-spec.json \
  http://198.51.100.248/api/v1/machines/mch_0f1e2d3c4b5a/install-plan
```

已验证(2026-09-20,eaf0ccd):`select size=largest` 解析到 LogicalDrive0;
分区形态 ESP(300M)+MSR(16M,自动插入)+ NTFS(Extend);`boot_drive=LogicalDrive0`。
文件分工:`/root/win2288h-spec.json` = job 载荷(POST /jobs 用,即
scripts/windows-dev/2288h-install.json);`/root/win2288h-plan-spec.json` = 裸 spec
(试算用)。发现过拒绝时响应是 422+分类码(eaf0ccd 修复前曾是 500)。
spec 无 keep,快照缺失不影响试算,但 **verify_layout 运行期仍需快照**
——discover(§1)不可跳过。

## 3. 提交安装

```bash
curl -s -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d @/root/win2288h-spec.json http://198.51.100.248/api/v1/jobs
```

观察:

```bash
curl -s -H "Authorization: Bearer $T" http://198.51.100.248/api/v1/jobs/<id> | python3 -m json.tool
curl -s -N -H "Authorization: Bearer $T" http://198.51.100.248/api/v1/jobs/<id>/events
tail -f /tmp/mammoth.log
```

阶段与时序预期:

1. **prepare_media(首次 ~10-20min)**:7z UDF 全树提取(~4.8G)→ wimlib 向
   SERVERSTANDARDCORE 注入 SetupComplete 对 → xorriso El Torito 保真重建
   (-udf -iso-level 3)。产物进 `MediaDir/pool-store/<iso-sha>/win/tree` 缓存
   (injector v1 + wim 字节数 marker),后续任务秒级复用。
2. **mount_media / boot**:iBMC 挂 boot-<token>.iso → SetBootDevice(CD, once) →
   引导。真固件 El Torito 直进 bootmgfw(无 TCG 交互窗口问题——qemu 收口结论)。
3. **install_os**:WinPE 读媒体根 autounattend.xml → 磁盘 phase 是 **LSI SAS3508
   inbox 驱动的第一验证点**:WinPE 若不识别,WillShowUI=OnError 会停在
   错误对话框、任务挂 install_os → KVM 看现场;预期 2019 inbox 驱动直接识别。
   镜像应用 ~10-25min(虚拟光驱带宽瓶颈,UOS 8.2G 用时 35min 可参照)。
4. **完成回调**:Setup 重启 → SetupComplete(SYSTEM,首登录前)读媒体上
   mammoth/task.json → 按 MAC 配 198.51.100.215 → POST CompleteURL(status=ok)
   → verify_ready 通过 → **回调释放介质**(task.json 消费窗口内介质不弹)。
5. **verify_ready**:机器未配 SSH 凭证 → 完成回调即验证面(既有语义)。

## 4. 验收清单(五条,对齐 test-baselines 口径的 windows 形态)

- [ ] 六阶段全绿零重试
- [ ] 完成回调 ok(248 侧 render 访问日志 / task 事件可见 POST 到达)
- [ ] 无人值守首启:KVM 控制台见登录界面(Ctrl+Alt+Del → Administrator)
- [ ] 装机凭据:Administrator 密码取 task.root_password 事件(一次性投递)
- [ ] 装后快照落库(verify_ready 带 SSH 凭证时;本机未配 → 以回调为准,跳过)

**LSI SAS3508 inbox 驱动验证(KVM 内)**:

```powershell
Get-Disk                                  # 3.64TB 卷在线,Initialize 状态符合预期
Get-PnpDevice -Class SCSIAdapter | Format-Table FriendlyName, Status
# 预期:LSI SAS3508 无黄色感叹号,驱动源 Microsoft 收件箱(非注入)
```

- 装后网络:`ipconfig` 应见 198.51.100.215/24 + 网关 198.51.100.1
  (SetupComplete 声明式配网的真机证据);248 侧 `ping 198.51.100.215` 通。

## 5. 预期外路径

- **WinPE 不识别 LSI 卷**(预期外):distros.md 既定后手 = boot.wim 驱动注入
  (v1.x);窗口内先以 KVM 截图 + `X:\Windows\panther\setupact.log` 留证收场。
- **构建水位拒斥**:`/data` 容量紧(2026-09-20 时 13G free,2022 ISO 已
  就位)。首建需 ≥9GiB,若拒斥,清理 win-dev rig
  (`/data/mammoth/win-dev/`,含 4.8G boot-dev.iso + qemu 盘)即解。
- **回调未达**:KVM 看登录界面是否出现;若已登录但 248 无回调,查
  SetupComplete 是否被首次登录打断(正常不会:SYSTEM 先于首登录)。

## 6. 窗口外待办(2026-09-20 收口后核销)

- ~~契约 `PartitionSpec.fs` 枚举补 `ntfs|fat32`~~ **✅ 已补**(eaf0ccd 同批)。
- ~~2022 ISO scp/核对~~ **✅ 已就位** `/data/os_iso/windows2022/`(5.5G,
  隐匿 El Torito 同 2019;2022 轮次随通路打通后作为构造变体验证)。
- ~~248 遗留死信 job 清账~~ 窗口日新增若干 canceled/failed(本窗口排障
  所致),清账顺延。
- iBMC 侧:Disk4 predictive failure / Disk1 abnormal 告警 + LSI Foreign
  configuration——盘健康核查,下次有人到场时处理。
