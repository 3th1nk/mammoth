# 11 · PXE 引导与零注册入门:场景与全链路导览

> 面向第一次接触 mammoth PXE 通路的人:两条机器入门路径的差异、一次网络引导
> 的完整接力、以及跨网段部署的边界与做法。机制细节在
> [06-install-pipeline.md](06-install-pipeline.md) §3.3 与
> [operations.md](operations.md) §4.5,本文不重复。

## 1. 两条入门路径:人找机器 vs 机器找平台

**正常流程是"人找机器"**:前提是人已经认识这台机器——手里有它的 BMC 地址
和账密。入口是带外(BMC),没有凭证,第一步就死:

```
POST /credentials(BMC 账密)
   └─ POST /machines(bmc.address + credential_id)──▶ 机器建档
        └─ 自动盘查(redfish,规格级)──────────▶ hardware 入档
             └─ 提交 install spec ────────────▶ 装机
```

典型走不下去的场景:新到货机器上架了但未录入资产(不知道哪台是谁、BMC 未
配置)、二手/回收机器、BMC 密码遗失——而这些机器的 PXE 网络引导通常是开着
的。**零注册入门是"机器找平台"**:机器插网线上电,固件走到网络引导项就会
广播 DHCP DISCOVER——这是机器在主动敲门,平台回应即可。

| | 正常流程 | 零注册入门 |
|---|---|---|
| 方向 | 自上而下:人拿资产信息注册机器 | 自下而上:机器自己冒出来 |
| 凭证 | 每台机器的 BMC 账密 | 部署级共享 token(`MAMMOTH_PXE_ENROLL_TOKEN`) |
| 身份 | `machines` 行(bmc_address 锚定) | `pending_machines` 行(MAC 锚定,**待认领**) |
| 数据 | redfish 带外,规格级 | /sys 内存环境,**分区级**(比注册后的 redfish 还深) |
| 电源控制 | BMC 上下电 | 无,探针自己 poweroff |
| 终点 | 可直接装机 | `claim` 升格为注册机器(见下) |

零注册**不装任何东西**:全程只读扫描 + 上报 + 关机。装机仍然必须回到正常
流程(挂介质、控电源都需要 BMC)。一句话:**正常流程要求机器先有"户口"
(BMC 凭证);零注册是在门口放登记本,新来的机器自己签到、留下体貌特征
(固件/硬件/盘),等你拿户口本来领。**

**claim(认领)** 把两条路径接起来:
`POST /api/v1/pending-machines/{mac}/claim`,body 与创建机器完全一致
(BMC 地址 + 凭证——登记本告诉你"这是谁",认领告诉平台"怎么找到它")。
服务端做四件事:①走与 POST /machines 完全相同的注册路径(含自动盘查);
②固件观测迁入机器的 PXE 观测列;③登记本里的 /sys 扫描迁为该机器的
**第一份 layout 快照**(source=ramdisk,[05-inventory.md](05-inventory.md)
§5 体系)——机器自己报的盘查不丢失;④消费台账行。BMC 地址冲突(认领时
才发现别人已注册)以 409 拒绝,台账行保留可查。

## 2. 一次网络引导的完整接力

PXE 引导是**逐级接力**:每一级引导程序只知道"下一级去哪",这个地址由
mammoth 通过 DHCP 应答递给它;主动发起请求的始终是机器侧的程序,mammoth
永远被动应答。

```
机器固件 PXE ROM(广播 DISCOVER,opt 60=PXEClient,opt 93=架构)
  │ ① mammoth proxyDHCP 应答:bootfile = undionly.kpxe(BIOS)/ shimx64.efi(UEFI)
  │    —— TFTP,ROM 只有 TFTP 栈,所以 NBP 只有百 KB 级
  ▼
NBP 加载执行,控制权交给 iPXE(现在跑在机器内存里,自带完整 HTTP 栈)
  │ ② iPXE 再次 DISCOVER(带 iPXE 标识)
  │    mammoth 识别出 iPXE → 应答不再给文件名,而是完整 URL:
  │    bootfile = http://<base>/netboot/script?mac=X&arch=Y
  │    (MAC 是 mammoth 从 DISCOVER 的 chaddr 读出后回填的,iPXE 只是
  │     忠实请求了服务器递给它的地址)
  ▼
③ iPXE HTTP GET /netboot/script?mac=X ──▶ 分派点,三种应答:
  │   ├─ 有条目(netboot_entries)→ 任务脚本(正常流程的装机/探针)
  │   ├─ 无条目 + enroll 开    → enroll 脚本(零注册,见 §3)
  │   └─ 无条目 + enroll 关    → exit 脚本,回固件引导序落本地盘
  ▼
④ kernel/initrd/modloop 全走 HTTP(自 iPXE 起才有 HTTP 栈——这就是
   "NBP 之后坚持 HTTP 分发"的由来,见 operations.md §4.5 第 2 条)
```

UEFI x64 的 Secure Boot 机器在 ① 处走的是 shim(Microsoft 签名)→
grubnet(Debian 签名)链,后续等价(grub 用 TFTP 拉渲染的
`grub.cfg-01-<mac>` 再走 HTTP),见 assets/pxe/PROVENANCE.md。

** DISCOVER 不是必然发生的**,有四个前提:固件 PXE 开启且在引导序里
(UEFI 下 IPv4/IPv6 是两个独立引导项)、轮得到网络引导(本地盘有可引导
系统且排在前面的机器永远到不了 PXE)、网卡带 PXE ROM、插在装机 L2。
"新到货 + 空盘 + 插网线"在实践中大概率会广播(固件找不到本地引导项自动
落空到网络),但这是运维经验不是机制保证。

## 3. 零注册链路的机制细节

```
未知机器 PXE ──▶ proxyDHCP 应答
   │                └─【第一次留痕】opt 93 读出 MAC+固件 → pending_machines
   │                    (机器只要 PXE 一次就被记账,无需引导探针)
   ▼
enroll 脚本(未知 MAC + MAMMOTH_PXE_ENROLL 开启时)
   │  kernel …/netboot/enroll-file/vmlinuz … enroll_mac=<mac>
   │  文件来自共享树 MediaDir/netboot/enroll(启动时构建一次,失败即
   │  fatal;载体复用 MAMMOTH_PROBE_ALPINE_NETBOOT/ISO)
   ▼
alpine 内存环境(与 ramdisk 探针同一套 /sys 扫描)
   │  共享 overlay 运行时读 /proc/cmdline 的 enroll_mac 拼进上报 URL
   │  ——共享树构建期不知道谁来,MAC 只能内核参数传递
   ▼
POST /netboot/enroll/{token}?mac=X ──▶ pending_machines.report 落库
   ▼
探针 poweroff(没有 BMC 可操作,软关机收尾)

管理员:GET /api/v1/pending-machines
   → MAC / 固件 / 盘查报告 / 首见时间
   → 拿序列号去机房认出机器,取得 BMC 账密 → 回正常流程注册(claim)
```

与 **ramdisk 探针**的区分:同一套载体与扫描,但 ramdisk 探针是**已注册
机器的任务内动作**(per-task 构建树、BMC 控制上下电、discover 任务轮询
结果);enroll 是**未注册机器的无主引导**(共享树、无任务、无 BMC)。

安全边界:`MAMMOTH_PXE_ENROLL_TOKEN` 必配(缺失启动即败,不提供默认值
——默认值必然公开,等于没有)。token 在 overlay 与脚本内核参数里,能触达
装机 L2 的人可伪造 pending 条目——低危(只污染台账,不触碰机器),与任务
引导树同一信任模型,见 operations.md §4.5 第 5 条。

## 4. 跨网段(跨 VLAN)与 DHCP Relay

**单播环节天然跨网段**:机器拿到引导信息后,TFTP 拉 NBP、HTTP 拉脚本与
kernel,全是发往 siaddr/ExternalURL 的单播,路由可达 + 防火墙放行即可。

**瓶颈只在最初一跳**:DISCOVER 是广播,不过路由器。跨 VLAN 需要 relay:

1. relay 的 ip helper 列表**加一项指向 mammoth 的 :67**(站点 DHCP 的
   helper 保留,两者并存是 PXE 的正常形态:站点 DHCP 给地址,proxyDHCP
   给引导信息);
2. mammoth 按 RFC 2131 §4.1 把应答发回 **giaddr:67** 由 relay 转回客户端
   L2(`bootReplyAddr` 的 giaddr 分支;单 L2 部署 giaddr 恒为 0,不走此
   分支)。giaddr 原样回显(RFC 2131 §4.3.1)。

> relay 路径已按协议实现并有单测钉住应答地址与 giaddr 回显;**真机 relay
> 拓扑尚未回归**(见 roadmap 的 PXE 真机矩阵余项)。qemu 验证可用网桥 +
> dnsmasq 做 relay 模拟(scripts/pxe-dev 同型)。

**当前最稳的跨 VLAN 拓扑**:mammoth 多网卡,每个装机 VLAN 插一条腿。
netboot 服务监听 :67/:69/:4011 是全接口的,一台 mammoth 天然服务多个装机
L2;"单 L2 单应答者"约束仍成立——每个 VLAN 只能有一个应答者,同机与站点
DHCP 不可共存(UDP 67 排他,见 operations.md §4.5 第 1 条)。

### 4.6 现场排障速查:PXE 客户端错误码与抓包入口(2026-09-19)

客户端侧错误码是第一诊断线索(网卡 ROM 在放弃前会告诉你死在哪一跳):

| 客户端报错 | 死点 | 典型原因 |
|-----------|------|---------|
| PXE-E53(No boot filename) | 收到 DHCP 但无 bootfile | proxy 模式无引导项(未武装/非白名单 MAC);外部模式站点 DHCP 缺 66/67 |
| PXE-E55(ProxyDHCP no reply on 4011) | 4011 无人应答 | proxyDHCP 未起/被防火墙挡;mammoth 未启用 PXE |
| PXE-E32(TFTP open timeout) | TFTP 首包拿不到 | next-server 不可达、TFTP 根目录缺文件、站点 DHCP option 43 编码错误把 NBP 路径弄丢(见下) |
| PXE-E99 / 卡图形 logo | NBP 拉完但跑不起来 | Secure Boot 拒签(应走 shim 链)、架构不匹配(arm64 机器发了 x64 NBP——option 93 观测可查) |

抓包永远是 PXE 排障的第一工具(比读 BMC/固件文档快):

```bash
tcpdump -ni <装机口> 'udp port 67 or udp port 69 or udp port 4011'
```

看四样:DISCOVER 是否到达(链路/ VLAN 问题)、OFFER 是否带 next-server+
filename( mammoth 应答内容)、TFTP RRQ 是否出现(引导项是否生效)、ACK0
是否缺失(X722 形态,tftp 容忍已覆盖)。

## 5. 外部 DHCP+TFTP:逃生门形态(2026-09-18)

builtin 模式的接力链(mammoth 亲历三跳:proxyDHCP→TFTP→HTTP)在 mammoth
拿不到特权 UDP 的部署形态下走不通。external 模式(`MAMMOTH_PXE_MODE=
external`)把前三跳交给站点 dnsmasq,mammoth 只剩 HTTP;接力点用**客户端
自报身份**缝合——静态 TFTP 蹦床(iPXE `${net0/mac}`、grub
`${net_default_mac}`)把 MAC 带进 URL,落回 builtin 的同两个 HTTP 端点。
部署细节与模式代价(option 93 观测失效)见
[operations.md](operations.md) §4.5.1;qemu 网桥 + dnsmasq 同型验证
(UEFI guest 经站点 DHCP+TFTP 完成 alpine agent 全装,六阶段绿)脚本在
`scripts/pxe-dev/external-e2e.sh`。
