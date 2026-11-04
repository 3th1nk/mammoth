# 13 · 无带外机器形态(headless)设计草案

> 状态:**设计草案,供排期评审**(2026-11-04;触发记录见 09-roadmap
> "办公/无 BMC 批量装机分场景重审定案")。评审通过后按"切片一/二"
> 实施,估四周末级(与 09-roadmap 2026-09-19 的预评估一致)。
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
