# 环境变量参考(引擎 + edge)

> 全部 `MAMMOTH_*` 变量的权威定义与校验逻辑:`internal/config/config.go`
> (引擎)与 `cmd/mammoth/edge.go`(edge);本文是运维视角的完整释义表。
> 组合规则(哪些形态配哪些段、互斥红线)见 `deploy/mammoth.env.example`
> 头部与 `deploy/README.md` §变量 × 形态矩阵;端到端照抄样本见离线部署
> 套件 compose/multi-segment/参数样本.md。
> 「适用形态」列:A = 单网段 all-in-one,B = faceted,C = 跨网段引擎,
> E = edge。空白默认值 = 功能关闭,语义见说明列。

## 1. Core(所有形态必配)

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_MODE` | `all` | A/B/C | 进程形态 `all\|api\|runner`(serve `--mode` 同名覆盖;compose 已按角色写死) |
| `MAMMOTH_DATABASE_URL` | (必填) | A/B/C | PostgreSQL ≥14 连接串:状态、凭证、任务队列的唯一强依赖 |
| `MAMMOTH_HTTP_ADDR` | `:8080` | A/B/C | HTTP 监听地址 |
| `MAMMOTH_EXTERNAL_URL` | `http://127.0.0.1:8080` | A/B/C | 机器/edge 可达的外部基地址:应答文件、介质池、iPXE 蹦床 chain、完成回调全走它;必须是机器网段可路由地址(B 跨机器 = LB VIP) |
| `MAMMOTH_API_TOKEN` | (必填) | A/B/C | 控制面 Bearer token(bin/mammoth 与集成方) |
| `MAMMOTH_MASTER_KEY` | (必填) | A/B/C | 凭证落库加密主密钥(`openssl rand -base64 32`);轮换 = 已存凭证不可解;B 跨机器全节点同值 |
| `MAMMOTH_LOG_LEVEL` | `info` | A/B/C | 日志级别 |
| `MAMMOTH_LOG_FORMAT` | `json` | A/B/C | `text` 人工 tail 更友好 |
| `MAMMOTH_METRICS_ADDR` | 空 | A/B/C | /metrics 独立监听;空 = 随 `HTTP_ADDR` 一并服务 |
| `MAMMOTH_OTEL_EXPORTER_ENDPOINT` | 空 | A/B/C | OTLP trace 导出端点;空 = 不导出 |
| `MAMMOTH_ENV_FILE` | 空 | A/B/C/E | env 文件路径(`--env-file` 同义);进程环境变量优先于文件条目 |

## 2. Storage(介质与 vMedia)

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_MEDIA_DIR` | `data/media` | A/B/C | 介质仓库(distro ISO 与解包树);B 跨机器 = 各节点 bind mount 同一 NFS 导出 |
| `MAMMOTH_MEDIA_WORKDIR` | 空 | A/B/C | 构建 scratch;空 = 紧邻介质目录的 `work/` |
| `MAMMOTH_MEDIA_BASE_URI` | 空 | A/B/C | 介质 NFS 导出地址,单旋钮双消费者(BMC vMedia 与 ubuntu casper nfsroot 必须同一服务端);空 = 不声明 NFS 源,vMedia 不可用;C 要求 BMC↔引擎可路由 |

## 3. PXE · 部署形态(MAMMOTH_PXE_MODE 单开关,三值)


| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_PXE_MODE` | `off` | A/B/C | **PXE 面单开关**:`builtin`(引擎托管 UDP——proxyDHCP+TFTP,设 `DHCP_POOL` 则池模式发租约;引擎须驻留机器 L2,host/macvlan 网络)\|`external`(引擎零 UDP,kit 照常导出,引导归站点 dnsmasq 或 edge,bridge 网络即可)\|`off`(纯 vMedia,仅 syslog sink);`BOOT_STRATEGY=pxe` 要求 builtin\|external;**external/off + DHCP_POOL 拒启**(红线 1) |
| `MAMMOTH_PXE_NEXT_SERVER` | 空 | A/B | 发给客户端的 next-server(须可达本机 HTTP);builtin 必需——EXTERNAL_URL 为 IP 字面量时自动派生;external 不使用(引导方在 edge/站点配置) |
| `MAMMOTH_PXE_DHCP_POOL` | 空 | A/B | 引导期地址池(逗号分隔;只对引导条目已装载/已 enroll 的 MAC 发租约);空 = 纯 proxyDHCP 不发租约;与站点 DHCP 并存 = pool+enroll 一体 OFFER(有竞速) |
| `MAMMOTH_PXE_DHCP_ROUTER` | 空 | A/B | 池租约网关;空 = NEXT_SERVER |
| `MAMMOTH_PXE_BROADCAST_ADDR` | 空 | A/B | PXE 应答广播地址;空 = 255.255.255.255;定向广播替换用于 macOS/vmnet 桥接(qemu 验证),Linux 无需 |
| `MAMMOTH_PXE_DHCP_PORT` | `67` | A/B | DHCP 监听端口 |
| `MAMMOTH_PXE_TFTP_PORT` | `69` | A/B | TFTP 端口 |
| `MAMMOTH_PXE_BOOTSRV_PORT` | `4011` | A/B | PXE boot-server discovery 端口 |
| `MAMMOTH_PXE_SYSLOG_PORT` | `514` | A/B/C | 安装器/探针日志 sink(UDP);0 = 按 514;绑不上仅降级警告;装机段防火墙须放行到引擎 |
| `MAMMOTH_PXE_UEFI_NBP` | `shim` | A/B | UEFI NBP 链:`shim`(签名,SB 有效)\\|`ipxe`(裸 iPXE,SB 须关——proxy 拓扑 UEFI 机型的 grub2 缺陷绕行);**external 下配 ipxe 拒启**(红线 2,该决策归 edge/站点 DHCP) |

## 4. PXE · 跨网段形态(edge 接客;与 §3 组合)

混合拓扑成立:引擎段 `builtin` 直服 + 其他装机段各放一台 edge——kit 由引擎统一
导出(`MediaDir/netboot/external-tftp`),edge 周期同步;地址服务按段各管(引擎
pool 只服务引擎段,edge 段归该段站点 DHCP;引擎池预留只写给本段机器)。远段无
站点 DHCP 的缺口 = edge-pool 候选(roadmap)。

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_PXE_WOL_PROXY_URL` | 空 | C | 跨段 WoL:引擎 boot 阶段 POST `{URL}/wol` 由 edge 代发 magic packet;空 = 不代发(人工上电自愈) |
| `MAMMOTH_PXE_WOL_PROXY_TOKEN` | 空 | C | 上面的 Bearer token,**与 edge `MAMMOTH_EDGE_WOL_TOKEN` 同值**(入脱敏表) |

## 5. 无带外认领(enroll→claim;叠加于 A/C)

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_PXE_ENROLL` | `false` | A/C | 零注册入口:未知 MAC 的 PXE 客户端经共享探针(alpine miniproot)报告进 pending_machines 台账;报告端点 `/netboot/enroll/<TOKEN>` 是网段内唯一门禁 |
| `MAMMOTH_PXE_ENROLL_TOKEN` | 空 | A/C | 上面的端点凭证;**ENROLL=true 而 TOKEN 空拒启**(红线 3);只配引擎一处,探针树构建时自动内嵌 |

## 6. 探针与无带外执行面

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_RAMDISK_ENABLED` | `false` | A/B/C | ramdisk 探针 discover 通路(probe: ramdisk)总开关;需 alpine ISO |
| `MAMMOTH_PROBE_ALPINE_ISO` | 空 | A/B/C | alpine standard ISO 路径/URL(探针/agent 介质 + apks);空 = 无 apks 降级 |
| `MAMMOTH_PROBE_ALPINE_NETBOOT` | 空 | A/B/C | alpine netboot tarball(PXE 探针/agent 载体必需——standard initramfs 缺机房网卡驱动);空 = PXE 探针树构建失败(启动即报错;enroll 同依赖) |
| `MAMMOTH_PROBE_DEBUG` | `false` | A/B/C | 探针 tty2 无鉴权调试 shell(busybox telnetd :2323);无串口机器的现场窗口,用完即关 |
| `MAMMOTH_PROBE_STATIC_CIDR` | 空 | A/B/C | 无 ssh.address 机器的探针寻址回退(静态 CIDR;空 = 走 DHCP) |
| `MAMMOTH_PROBE_GATEWAY` | 空 | A/B/C | 上述回退的默认路由(跨段报告目标) |
| `MAMMOTH_PROBE_PREFIX` | `24` | A/B/C | 上述回退的前缀长度 |
| `MAMMOTH_PROBE_WAIT` | `10m` | A/B/C | 探针报告等待预算 |
| `MAMMOTH_FAKE_DISK_GBYTES` | 空 | — | 工控机 POC 过渡(fake 盘容量;正式部署勿用) |

## 7. 发行版介质与安装源

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_PXE_DI_NETBOOT` | 空 | A/B/C | debian installer(d-i)netboot tarball;空 = debian PXE 通路未备 |
| `MAMMOTH_PXE_DI_UDEBS_DIR` | 空 | A/B/C | d-i 额外 udeb 目录 |
| `MAMMOTH_UBUNTU_APT_MIRROR` | 空 | A/B/C | ubuntu autoinstall 的 apt 镜像;空 = archive.ubuntu.com(离线段卡 subiquity 镜像检查对话框,2288H 实证);中国部署 mirrors.aliyun.com/ubuntu/ |
| `MAMMOTH_WINDOWS_INSTALL_SMB_UNC` | 空 | A/B/C | wimboot setup 通路的 SMB 只读导出 UNC(反斜杠原样);空 = setup 通路不可用;agent/auto 不走 SMB |
| `MAMMOTH_WINDOWS_INSTALL_SMB_USER` | 空 | A/B/C | SMB 用户;空 = guest |
| `MAMMOTH_WINDOWS_INSTALL_SMB_PASSWORD` | 空 | A/B/C | SMB 密码 |
| `MAMMOTH_WINDOWS_APPLY_ALPINE_ISO` | 空 | A/B/C | windows agent apply-image 通路的 alpine extended ISO(免 SMB);空 = 通路未备 |

## 8. 执行面(BMC/装机;B 形态配在 runner)

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_BMC_TLS_INSECURE` | `false` | A/B/C | 接受自签证书(工厂 BMC 普遍无信任链) |
| `MAMMOTH_BMC_TIMEOUT` | `30s` | A/B/C | BMC 动作等待预算;华为 iBMC vMedia 挂载 12-15s + Connect 概率失败,建议 90s |
| `MAMMOTH_INSTALL_CONSOLE` | `serial` | A/B/C | 安装器控制台:`serial`(SOL 运维面)\\|`vga`(KVM 调试看画面) |
| `MAMMOTH_IPMI_INTERFACE` | `lanplus` | A/B/C | ipmitool 接口形态 |
| `MAMMOTH_BOOT_SETTLE_DELAY` | `0s` | A/B/C | boot 后沉淀等待 |
| `MAMMOTH_INBAND_TIMEOUT` | `20s` | A/B/C | 带内操作(SSH)预算 |
| `MAMMOTH_VERIFY_READY_WAIT` | `10m` | A/B/C | 装后核验等待 |
| `MAMMOTH_RESCUE_WAIT` | 空 | A/B/C | 救援报告等待独立预算;空 = 运行时回退 25m(真机实证 10m 杀死引导中 agent) |
| `MAMMOTH_BIOS_CONFIRM` | `required` | A/B/C | BIOS 档案提交期确认门 `required\\|optional` |
| `MAMMOTH_ERASE_CONFIRM` | `required` | A/B/C | 擦盘提交期确认门 `required\\|optional` |

## 9. 队列与可观测(默认即生产形态)

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_RUNNER_CONCURRENCY` | `10` | B(runner) | runner 任务并发 |
| `MAMMOTH_RUNNER_MAX_TASK_ATTEMPTS` | `5` | B(runner) | 任务级重试上限 |
| `MAMMOTH_VISIBILITY_TIMEOUT` | `30s` | A/B/C | 队列可见性超时(超时未 ack 重新投递) |
| `MAMMOTH_REAPER_INTERVAL` | `15s` | A/B/C | reaper 轮询(崩溃任务 interrupted 标记) |
| `MAMMOTH_QUEUE_POLL_INTERVAL` | `250ms` | A/B/C | 队列收件轮询间隔 |
| `MAMMOTH_QUEUE_RETRY_BACKOFF` | `5s` | A/B/C | 重试退避 |
| `MAMMOTH_QUEUE_MAX_RECEIVE_COUNT` | `5` | A/B/C | 最大接收次数(超出进死信) |
| `MAMMOTH_HEARTBEAT_INTERVAL` | `10s` | A/B/C | runner 心跳间隔 |
| `MAMMOTH_HEARTBEAT_TIMEOUT` | `60s` | A/B/C | 心跳超时判定 |
| `MAMMOTH_TASK_STATUS_INTERVAL` | `2s` | A/B/C | 任务状态上报间隔 |
| `MAMMOTH_TASK_LOGS_TTL` | `2160h` | A/B/C | 任务日志保留(90 天) |
| `MAMMOTH_LAYOUT_RETENTION` | `10` | A/B/C | 每机 layout 快照保留份数 |
| `MAMMOTH_IDEMPOTENCY_TTL` | `24h` | A/B/C | 提交幂等窗口 |
| `MAMMOTH_MEDIA_RELAY_ADDR` | 空 | A/B/C | 介质中继(BMC 直挂不可达时代理;ADDR 空 = 整组关) |
| `MAMMOTH_MEDIA_RELAY_DIR` | 空 | A/B/C | 中继导出目录 |
| `MAMMOTH_MEDIA_RELAY_USER` | 空 | A/B/C | 中继凭证 |
| `MAMMOTH_MEDIA_RELAY_PASSWORD` | 空 | A/B/C | 中继凭证 |
| `MAMMOTH_FAKE_BMC_DELAY` | 空 | — | fake BMC 动作延迟(POC) |
| `MAMMOTH_COMPAT_DIR` | 空 | A/B/C | compat 资料目录覆盖(空 = 随安装前缀) |

## 10. edge(只认 `MAMMOTH_EDGE_*`;见 deploy/edge.env.example)

| 变量 | 默认值 | 形态 | 说明 |
|---|---|---|---|
| `MAMMOTH_EDGE_BASE_URL` | (必填) | E | 引擎机器面基地址:kit 同步 + sightings 回传源(与引擎 EXTERNAL_URL 同值) |
| `MAMMOTH_EDGE_NEXT_SERVER` | 空 | E | 发给客户端的 siaddr/TFTP-server(**edge 自己的装机段地址,不是引擎**);空 = 从首个 PXE 请求到达接口自动学习;装机窗口内地址不得变化 |
| `MAMMOTH_EDGE_UEFI_NBP` | `shim` | E | UEFI NBP 链(shim\\|ipxe),判据同 §3;站点 DHCP 不可控拓扑的 UEFI 机型切 `ipxe`(SB 关) |
| `MAMMOTH_EDGE_KIT_TOKEN` | 空 | E | 引擎 kit 端点 Bearer token;空 = 匿名(/netboot 默认公开) |
| `MAMMOTH_EDGE_SEGMENT` | 空 | E | sightings 回传携带的网段标签(信息性) |
| `MAMMOTH_EDGE_TFTP_ROOT` | `/var/lib/mammoth/tftp` | E | kit 落位目录(启动 + 周期同步,ETag 协商,原子换根) |
| `MAMMOTH_EDGE_SYNC_INTERVAL` | `5m` | E | kit 重同步周期;`0` = 仅禁周期(启动初始同步仍执行) |
| `MAMMOTH_EDGE_DHCP_PORT` | `67` | E | proxyDHCP 监听端口 |
| `MAMMOTH_EDGE_PROXY_PORT` | `4011` | E | boot-server discovery 端口 |
| `MAMMOTH_EDGE_TFTP_PORT` | `69` | E | TFTP 端口 |
| `MAMMOTH_EDGE_SYSLOG_PORT` | `514` | E | 安装器日志 sink(绑定失败仅警告) |
| `MAMMOTH_EDGE_WOL_LISTEN` | 空 | E | WoL 中继 HTTP 监听(如 `:9100`);空 = 关(人工上电自愈) |
| `MAMMOTH_EDGE_WOL_TOKEN` | 空 | E | /wol 的 Bearer token;空 = 无鉴权(装机 L2 即信任边界);**与引擎 WOL_PROXY_TOKEN 同值** |
| `MAMMOTH_EDGE_WOL_BROADCAST` | `255.255.255.255` | E | magic packet 广播地址(装机段定向广播最精确) |
