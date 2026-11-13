# PXE 引导卡死 / 待认领为空 · 诊断 runbook

> 适用现象:机器开了 PXE 引导并重启,但待认领列表(pending_machines)
> 不出现该机器,或出现 sighting 后引导链不再推进(无 TFTP RRQ、无脚本
> fetch)。首次实战:2026-11-12 办公 PC(172.16.20.31),从"引擎盲"
> 定位到"严格型 ROM 拒收分体 OFFER",两次环境误判被本流程的差分探测
> 纠正。背景矩阵与处置见 operations.md §站点 DHCP 共存节。

## 第 0 步 · 引擎形态三问

先回答三个问题(容器的 env + 启动日志行),后面每一步的判读都依赖它:

```sh
docker inspect <engine> --format '{{range .Config.Env}}{{println .}}{{end}}' | grep MAMMOTH_PXE
docker logs <engine> 2>&1 | grep -E "netboot service started|external PXE kit|enrollment tree"
ss -ulnp | grep -E ':(67|69|4011|514) '
```

| 形态 | 启动日志 | UDP 端口 |
|---|---|---|
| builtin proxy | `netboot service started`(无 pool 字样) | 67/4011/69/514 全监听 |
| builtin pool | 同上(env 有 `MAMMOTH_PXE_DHCP_POOL`) | 同上 |
| external | `netboot service started (external: ...)` | 仅 514(syslog) |
| PXE 关 | `syslog-only` 字样 | 仅 514 |

external/SyslogOnly 形态引擎不碰 DHCP——sighting 只能来自 edge 回报、
iPXE 脚本兜底或 enroll 探针;裸 PXE ROM 无反应是形态使然,不是故障。
(`MAMMOTH_PXE_ENROLL` 是否开启决定"未知 MAC 进 pending"的脚本兜底
是否存在;pool 模式 + enroll 还决定一体 OFFER 门禁,见第 4 步。)

## 第 1 步 · 日志分水岭

重启目标机,同时盯引擎日志:

```sh
docker logs -f <engine> 2>&1 | grep -E "dhcp: request|not leasing|sighting"
```

- **有 `dhcp: request from ... -> reply to ...`** → DISCOVER 到了引擎,
  引擎已应答;问题在下游,进第 2 步(ROM 收到没有?接了吗?)。
- **什么都没有** → 包根本没到引擎:同 L2 吗(ARP/`ip addr` 对网段)?
  external 模式吗(回第 0 步)?防火墙(UDP 67/4011/69)?网线/Wi-Fi
  (PXE 只走有线口)?重启时引擎活着吗(PXE 只在开机几秒内重试)?
- **有 `no armed entry for <mac> — not leasing`** → pool 模式拒答:
  未知 MAC 且 enroll 未开/未见过的普通客户端。想要零注册入口就开
  `MAMMOTH_PXE_ENROLL`(v1.4.4 起 pool+enroll 对 sighting 过的 MAC 发
  一体 OFFER)。

## 第 2 步 · 线上抓包(注意盲区)

在引擎宿主机抓,过滤 DHCP+PXE 全端口:

```sh
timeout 240 tcpdump -i <nic> -n -e -w /tmp/pxe.pcap \
  'udp port 67 or udp port 68 or udp port 69 or udp port 4011'
```

**盲区警告(2026-11-12 教训)**:tcpdump 只能看到本机端口的帧。站点
DHCP 对客户端的应答通常是 **L2 单播**(直接经交换机转发给客户端 MAC),
在第三方端口上**永远抓不到**——"抓包里站点 DHCP 沉默"不成立,它可能
已经应答了客户端。判断站点 DHCP 行为必须用第 3 步的主动探测,不能只
靠旁观抓包。

判读要点:
- 引擎的 OFFER 在线上吗(有 DISCOVER 就应有 REPLY,间隔 ms 级)?
- 客户端随后有 REQUEST(认领租约)吗?有 = 租约阶段通了;
- 有 TFTP RRQ(udp 69)吗?有 = ROM 阶段通了,卡点在 NBP 之后
  (grub2/KB-00043 边界:RRQ shim/grub 成功然后静默 = 该 bug);
- 全都没有 = ROM 收到 OFFER 但不认,进第 3 步。

## 第 3 步 · 差分探测(判站点 DHCP 行为)

从任一台 L2 内主机(引擎宿主机即可),用**假 MAC** 发两类广播 DISCOVER,
对照站点 DHCP 的反应:

```sh
# 构包(240B 头 + cookie + options),发送:
socat -u FILE:/tmp/dhcp-disc.bin UDP-DATAGRAM:255.255.255.255:67,broadcast
# 注意:bash 的 /dev/udp 不带 SO_BROADCAST,广播发送会 EACCES;必须 socat。
```

| 探测 | option 60/93 | 结论 |
|---|---|---|
| 普通客户端 | 无 | 有 OFFER = 站点 DHCP 服务本段且不排斥陌生 MAC |
| PXE 客户端 | 60=`PXEClient:Arch:00007`、93=7 | 有 OFFER = 不做 PXE 过滤;无 = 仅过滤 PXE(少见) |

两类都应答(2026-11-12 实测:3-8ms,scope 从 .32 起发)则站点 DHCP
行为健康,ROM 拒收分体 OFFER 的嫌疑上升;配合第 2 步"零 RRQ/零
REQUEST"即闭环 → 严格型 ROM,处置见第 4 步。

探测纪律:假 MAC 会在站点 DHCP 留下 12h 幽灵租约(占用 scope 一个
地址),探测用包要少而准。

## 第 4 步 · 根因判定表

| # | 证据组合 | 根因 | 处置 |
|---|---|---|---|
| 1 | 引擎无 DISCOVER 日志 | L2/防火墙/形态错 | 第 0/1 步修正 |
| 2 | 引擎应答了,ROM 零 RRQ,站点 DHCP 差分应答健康 | 严格型 ROM 拒分体 OFFER | operations.md §共存矩阵:A(66/67 静态绑定)或 B(pool+enroll);办公网先核 snooping |
| 3 | ROM 取了 NBP,shim/grub RRQ 成功后静默 | grub2 不处理 UEFI ProxyDHCP(KB-00043) | SB-off 切 `MAMMOTH_PXE_UEFI_NBP=ipxe`;SB-on 无解,走 vMedia |
| 4 | 有 RRQ 到错误 IP | ROM 信了站点 OFFER 的自指 siaddr | 66/67 明示引导服务器 |
| 5 | pool 模式 `not leasing` 日志 | 门禁正常拦截 | 开 enroll(pending 台账 MAC 放行)或按流程登记 |

## 第 5 步 · 处置后验证

修复动作落完后,合成探包(假 MAC PXE DISCOVER,broadcast flag 置位)
→ 引擎日志出现应答 → pending 台账出现该 MAC(`pending.sighted` 事件)
→ 真机重启走全链。全链通过后记得清理探针 sighting(DELETE
`/pending-machines/<mac>`,台账事件 `pending.dismissed` 可审计)。
