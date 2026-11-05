# 13 · 无带外机器形态(headless)设计草案

> 状态:**代码面已实施(2026-11-04,feat/headless 分支),真机验证待窗口**
> (2026-11-04;触发记录见 09-roadmap "办公/无 BMC 批量装机分场景重审
> 定案")。切片一/二/三的主体已按本稿落地(实施映射见 §8),剩余:
> 工控机真机端到端(注册→claim→PXE 装机→WoL 唤醒)与 qemu 验证轮。
> 首个用例:工控机 POC(x86_64,Ubuntu 24.04,单网段,pxe-proxy/池
> 双兼容)。

## 1. 目标与非目标

**目标**:让无带外管理(BMC)的 x86_64 机器走完既有六阶段装机流水线
——身份登记、盘查、引导、装机、验证全自动(保留人工重启兜底)。

**非目标**:
- Windows/应用镜像克隆(imaging 范式,维持排除,见 09-roadmap 重审定案);
- 消费硬件 WinPE 驱动注入;
- 跨网段无带外(WoL 是 L2 广播,代理机须与机器同段——与 pxe-proxy
  落点原则一致);
- 机房级电源编排(配电/PDU 层面)。

## 2. 机器契约:`bmc.protocol = none`

```jsonc
{
  "bmc": { "protocol": "none" },        // 新枚举值;address/credential 不需要
  "identity": { "hostname": "..." },     // 不变
  "network": [ { "match": { "mac": "..." }, ... } ]  // 不变;MAC 是机器身份
}
```

- **身份 = MAC**:装机网段唯一的机器标识(netboot_entries 按 MAC 建档,
  enroll/pending_machines 按 MAC 登记——既有机制全部复用);
- `capabilities` 对 none 机器诚实降级:无固件/健康/SEL/BIOS/擦盘
  (探针健康面仍可用——它是带内采集,见 05-inventory);
- 状态机沿用:redacted "discovering → ready → installing → ready";
  `power_state` 恒 `unknown`(诚实:引擎对 none 机器没有电源视图);
  借鉴 docs/05 的"无证据不发明结论"。

## 3. 登记与盘查(两条通路)

### 3.1 enroll 零注册 → claim(推荐,已有设施)

`MAMMOTH_PXE_ENROLL=true` 时,未知 MAC 的 PXE 客户端收到共享探针
载荷(alpine),扫 /sys + SMART + NIC 后报告进 pending_machines
——**已实现**。新增:

- **claim 时报告固化**:`POST /pending-machines/{mac}/claim` 把探针
  报告(disks/NIC/memory)固化为机器的 inventory 快照与初始 layout
  (与 redfish 探针同形;`coverage: partial` 标注——盘序列号/固件
  等带外专属字段为空,不发明结论);
- claim 请求带 `bmc.protocol: none` 显式确认(防误升级)。

### 3.2 手工注册 + 现场解析(兜底,POC 现行方案的正式化)

直接注册 protocol=none + 手填磁盘容量(install-spec 的
`root_device_hints`/selector 按容量匹配;装机现场 size±1% 解析
——既有机制,autoinstall resolve-disk 与 agent resolve_disks 双通路
已验证)。工控机 POC 用此通路(fake 变通即本通路的临时实现,
headless 落地后切换)。

## 4. boot 阶段:`none` 的 arm 语义

现有 bootStage arm = BMC power_off → set_boot_device(PXE, oneshot) →
power_on。none 机器拆为:

```
arm(none):
  1. 注册 netboot 引导项(不变;netboot_entries 按 MAC,幂等)
  2. 若机器离线(最近 sighting 超 N 分钟):发 WoL magic packet
     (UDP 广播至装机网段 ×重复 3 次;目标 MAC = 机器身份 MAC)
  3. 进入等待:install_os 以「引导证据」推进而非电源回调——
     证据源 = DHCP 池租约反查 / enroll sighting / pxe-proxy
     sightings(三者皆既有);收到即认为引导已发生
  4. 超时兜底:引导等待窗口(默认 10 分钟,spec 可调)无证据 →
     任务日志 WARNING("请人工重启机器")→ 继续等(人工重启后
     引导证据自然出现,流程自愈);INSTALL_TIMEOUT 上限不变
```

- **重装场景**:机器通常在线(SSH 可达),arm 时先经带内优雅重启
  (SSH reboot)再走 2——无 BMC 但装好的机器有 SSH,重启可控;
- **verify_ready**:完成回调 + SSH 探活皆不依赖 BMC ✓ 原样;
- **fake 兼容**:protocol=fake 保留(dev 测试形态);none 是生产
  通路,两者不混用。

## 5. 安全边界

- enroll token 必配(既有);claim 是控制面操作(Bearer token 既有);
- none 机器的 netboot 文件面沿用 allowlist + token(不变);
- **新增面**:WoL 发包地址 = 装机网段广播——引擎须在机器 L2(单网段)
  或由 pxe-proxy 代发(跨网段:边缘代理增 WoL 转发端点,代理本就在
  机器 L2;切片二);
- 身份伪造:MAC 可伪造(装机 L2 内攻击者已有该能力,威胁模型不变,
  docs/security-baseline "装机 L2 内"前提)。

## 6. 切片与估时

| 切片 | 内容 | 估时 |
|---|---|---|
| 一(核心) | protocol=none 契约 + enroll→claim 报告固化 + 手工注册容量通路 + boot none 语义(等待引导证据/人工兜底) + 工控机真机验证 | 2 周 |
| 二(WoL+跨网段) | WoL 发包(引擎直发)+ pxe-proxy 代发端点 + 跨网段真机回归 | 1 周 |
| 三(打磨) | capabilities 降级面/文档/runbook/验收套件 | 1 周 |

依赖:无新外部组件;alpine 探针/resolve 双通路/sightings 全为既有设施。
首验机器:工控机(POC 机器,Ubuntu 24.04,单网段)。

## 7. 风险

- **BIOS 差异长尾**:工控机 BIOS 的 PXE 支持度参差(POC 确认清单
  B2/B3 是硬前提)——不可用时无解(退路 U 盘/带 BMC 机型);
- **WoL 不可达**:跨网段/路由器过滤广播 → 切片二的代理代发解决;
  单网段直发通常可用;
- **机器中途断电**:tmpfs 装机环境全丢 → 重新引导即重来(无状态,
  天然幂等);agent 载体的 bail 诊断链已为此设计;
- **fake 通路共存**:POC 期间的 fake 注册机器与 none 机器并存——
  headless 落地后 POC 机器迁移到 none,fake 保留为 dev 形态。

### 7.1 WoL 兼容性边界(2026-11-04 澄清,记录口径)

**发送侧零区分**:AMD magic packet(6×FF+16×MAC)是链路层事实标准,
UDP 封装(port 9)与裸 Ethernet 帧(0x0842)NIC 均认载荷模式——按芯片
(Intel/Realtek/Broadcom)或主板厂商均无需区分,本引擎的发送实现一层
不用改。规范内唯一可选扩展 SecureOn(附 6 字节密码)仅极少数企业环境,
不实现。

**差异全在接收侧五层**:
1. 网卡芯片——只区分**附加唤醒模式**(仅 magic/+单播/+ARP/pattern),
   magic 是全员基线;
2. 主板——断电供电电路与 PME 路径(PCIe 时代免 WOL 线);
3. BIOS/UEFI——**ErP/S5 深睡开启 = 软关机后 NIC 完全断电,WoL 必死**
   (现场 WoL 失效第一大因);WoL/PME Wake 开关;来电自启(G3 物理断电
   任何 WoL 无解,只能来电自启或人工);
4. OS 驱动——**Linux 的 ethtool WoL 默认 `d`(禁用)**:装好的 Linux
   被软关机后 NIC 不再监听,下次 WoL 无效(Windows 驱动默认开);
5. L2 拓扑——广播不可路由(路由器默认拒定向广播),跨段必须代理代发
   (已实现)。

Intel 专项:vPro/AMT 平台**不属于 headless 场景**(ME 固件自带带外管理,
应走 AMT 的 Redfish 通路,能力远超 WoL);个别早期 2.5G 型号(I225
早期步进)有"WoL 唤醒后链路不恢复"勘误,靠 NIC 固件更新解决;Intel
消费级主板 BIOS 的 ErP 默认开启常见,排障第一查。

**打磨待办(暂不实施)**:
- POC 确认清单增列 WoL 支持确认项(BIOS WoL/PME Wake 选项存在性、
  ErP 可关),与 C4(来电自启)并列;不做也不阻塞(人工重启 C1 本身
  是兜底,headlessArm 的 WoL 失败仅 WARN 落人工路径);
- 装好的系统固化 NIC armed:装后配置 systemd .link `WakeOnLan=magic`
  (ethtool 语义),保证软关机后的重装/唤醒场景仍可 WoL——装机流程走
  reboot 不软关机,影响面小,属打磨项。

## 8. 实施记录(2026-11-04,feat/headless 分支)

| 切片 | 内容 | 落点 |
|---|---|---|
| 一 | protocol=none 契约(迁移 00011 / BMCProtocol 枚举 / none://<mac> 身份 / claim 显式确认) | b9891ca |
| 一 | enroll 探针 NIC 采集 + claim 固化 hardware 视图(coverage=partial;PXE 观测与 netboot 武装的承重墙) | 952f245 |
| 一 | boot none 语义(headlessArm:证据驱动 / 带内优雅重启 / 人工兜底自愈 / BOOT_EVIDENCE_TIMEOUT);顺带修迁移遗漏的 scanMachine NULL 扫描 | 0fb6130 |
| 二 | WoL 全链:AMD magic packet(golden)+ 引擎直发(MAMMOTH_PXE_WOL_BROADCAST)+ pxe-proxy 代发端点(--wol-listen,跨网段) | f2ba497 |
| 三 | 能力降级门禁:控制器动作/同步 BMC 读 422 HEADLESS_UNSUPPORTED;提交期 PXE-only 与 hardware-RAID 拒绝(任务级预失败,不阻塞同批) | cce0293 |

证据语义(§4.3 的实现形态):**arm 水位后的 PXE sighting**——builtin 模式
读 machines.pxe_last_seen_at(proxyDHCP DISCOVER 经 ObservePXE 按 hardware
nics 匹配,claim 固化的 NIC 视图是前提),external 模式读 pending 台账
(代理 sighting feed);DHCP 租约刻意不作证据(池租约粘性跨轮存活)。
等待窗口 spec 可调(boot.evidence_wait_seconds,默认 600s),窗口过发
task.boot_manual_required 事件并继续等(人工上电自愈),任务超时为总上限。

部署面文档:docs/operations.md §4.8(headless 部署:注册/claim/WoL 配置/
proxy 代发/门禁语义)。

**剩余(真机窗口)**:工控机端到端(enroll→claim→install→WoL/人工兜底
各一轮)+ qemu 形态验证轮(enroll 开启的 external rig + claim 提交);
console 仓的 none 协议展示(设置页协议枚举/机器详情降级面)随 POC 后跟进。
