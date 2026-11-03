# pxe-proxy 真机验证矩阵(proxyDHCP + 不可控站点 DHCP 共存)

背景与机制:docs/operations.md §pxe-proxy(站点 DHCP 共存节)、
docs/11-pxe-walkthrough.md §4。目的:把"pxe-proxy + 站点 DHCP 不可控"
这一跨网段形态的能力边界做实——上游硬边界(grub2 不处理 UEFI 模式的
Proxy DHCP 消息,RackN KB-00043 / RH Bugzilla #1809246)要求按机型固件
分型验证,而不是把该形态当成一个整体判定可用/不可用。

## 前置条件(模拟站点 DHCP 的环境卫生)

- dnsmasq 严格"仅地址池":配置不得含 `dhcp-boot`、`pxe-service`、
  `enable-tftp` 任何一行——残留会把客户端引向不存在的 TFTP(FOG 社区
  常见 "client pulls from the wrong IP",见 operations.md 共存节);
- pxe-proxy 独立主机,与机器同 L2(UDP 67/69/4011;与站点 DHCP 同机会
  bind 冲突);`--next-server` = 该 L2 本机地址;
- 引擎机器面 8080(TCP)从装机 VLAN 可路由(kit 同步 + sightings 回传 +
  iPXE 脚本 fetch 共用);
- 引擎侧已武装 netboot 条目(或开 `MAMMOTH_PXE_ENROLL`)。

## 实验矩阵(按序执行,前一步通过才进下一步)

| # | 拓扑 | 机型固件 | NBP 配置 | 预期 | 通过判据 |
|---|------|---------|----------|------|---------|
| 1 | 同 L2 dnsmasq(IP 池)+ pxe-proxy | BIOS | 默认 | 可用——iPXE re-DHCP 是 proxyDHCP 的参考客户端,主验证路径 | 四跳(DISCOVER OFFER→TFTP undionly→HTTP 脚本→kernel/initrd)全绿 → 任务 succeeded |
| 2 | 同上 | UEFI x64 SB-off | `--uefi-nbp=ipxe` | 可用——裸 iPXE 绕开 grub2 缺陷 | 同上 |
| 3 | 同上 | UEFI x64 SB-on | 默认(shim 链) | **预期失败**:grub2 不处理 Proxy DHCP | 抓到下方判别表 #1 签名 → 边界定案,写 compat |
| 4 | relay 拓扑:helper 列表含 dnsmasq 与 pxe-proxy 两处 | 1/2 已通过的固件 | 同 1/2 | giaddr 回程真机回归(roadmap 挂账项) | 同 1/2,且跨段无额外失败形态 |

## 失败形态判别(抓包四看 + 客户端屏幕错误码)

| # | 症状 | 根因 | 判别点 |
|---|------|------|--------|
| 1 | shim/grub 的 TFTP 传输成功 → 静默,机器落 GRUB 提示符 | grub2 不处理 UEFI Proxy DHCP(KB-00043) | tcpdump:TFTP 传输完成后无 grub.cfg RRQ、无 HTTP 请求 |
| 2 | 拿到地址后 PXE-E53(No boot filename received) | DISCOVER 没到 pxe-proxy,或站点 OFFER 被选中但无引导信息 | pxe-proxy 日志有无 `dhcp: request from`;引擎 pending_machines / machines.pxe_firmware 有无 sighting |
| 3 | 客户端 TFTP RRQ 目标 = 站点 DHCP 的地址 | 站点 siaddr 默认自指 + 粗心 ROM | 抓包对比两份 OFFER 的 siaddr 与实际 RRQ 目标 |
| 4 | PXE-E32 / E38(TFTP 超时) | next-server 不可达(地址 / 端口 / 防火墙) | pxe-proxy 启动日志 `next_server=`;UDP 69 放行;从机器 L2 `tftp get` 探测 |
| 5 | pxe-proxy 起不来(bind: address already in use) | 与站点 DHCP 同机,UDP 67 冲突 | 进程日志;pxe-proxy 必须独立主机 |

排障第一检查点永远是 **sighting 有无**(operations.md 共存节):无
sighting 是"包没到"(helper 列表 / L2 归属 / 端口冲突),有 sighting 而
引导失败才是下游(TFTP 可达性 / grub2 边界 / 蹦床 HTTP 可达性)。

## 结论落点

- 通过项:按固件分型写进 docs/compat 对应机型条目(SB-off 机型注明
  `--uefi-nbp=ipxe` 部署口径);
- 失败定案:SB-on + 不可控站点 DHCP 的边界已在 operations.md 共存节
  (行业定案 + 本地实证);不要在该组合上反复消耗排障时间——出路的优先
  级:谈判站点 DHCP 加 option 66/67 > vMedia 载体 > UefiHttp;
- relay 项:实验 4 的结果回写 docs/11-pxe-walkthrough.md §4 与 roadmap
  的真机 relay 回归挂账项。
