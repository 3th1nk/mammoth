# 运维手册:备份恢复与日常运营

> v1.0 门槛项之一(docs/09-roadmap.md:备份恢复文档)。本文描述 mammoth 的
> 状态边界、备份策略与恢复流程。

## 0. 配置入口

- 全部配置为 `MAMMOTH_*` 环境变量(12-factor);完整清单以
  `internal/config/config.go` 为准(带默认值与语义注释);
- **dotenv 文件**:裸二进制部署可用 `serve --env-file /etc/mammoth.env`
  (或 `MAMMOTH_ENV_FILE=`)——文件**播种**环境,进程已有变量**优先**于
  文件;非法值(非整数/非布尔/非时长)**启动即报错**并聚合列出全部问题,
  不静默用默认值;
- systemd 部署等价物:`EnvironmentFile=`;compose:`env_file:`。
- 可选探针特性:ramdisk 探针(discover `probe: ramdisk`)的启用清单见
  [05-inventory.md](05-inventory.md) §4;PXE 零注册入门见本文 §4.5 第 5 条。

## 1. 状态边界:什么需要备份

| 组件 | 状态 | 备份必要性 |
|------|------|-----------|
| **PostgreSQL** | 唯一权威状态:资源、凭证密文、任务、事件、队列 | **必须**(全部状态都在这里) |
| **MAMMOTH_MASTER_KEY** | 凭证静态加密主密钥(AES-256-GCM) | **必须**——密钥丢失 = 所有凭证永久不可解 |
| 介质仓库(本地卷/对象存储) | 任务引导介质(boot-<token>.iso)与渲染产物(渲染快照同时持久化在 DB) | 建议(可重建,但耗时) |
| mammoth 二进制/容器 | 无状态,可随时重建 | 不需要 |

**核心结论:状态 = 数据库 + 主密钥。** 两者缺一,系统不可恢复。

## 2. 备份策略

### PostgreSQL

```bash
# 逻辑备份(推荐,跨版本可移植)
pg_dump --format=custom --file=mammoth-$(date +%F).dump "$MAMMOTH_DATABASE_URL"

# 持续归档(WAL):生产环境建议配合 pgBackRest / WAL-G
```

要点:

- `queue_messages` 中的在途消息随库备份;恢复后可见性超时会让未 Ack 的
  消息重新投递(至少一次语义),无需特殊处理;
- `layout_snapshots` 是不可变追加表,随库备份即可,无需单独策略;
- `events` 默认保留 30 天,不需要长期归档(审计另有需求时单独导出)。

### 主密钥

- `MAMMOTH_MASTER_KEY` 是 base64 的 32 字节密钥,由部署方生成与注入;
- **备份到与数据库不同的位置**(KMS、保险柜、离线介质)——同一泄露域内的
  "数据库+密钥"备份等于没有加密;
- 轮换:当前版本不支持在线轮换;轮换需解密-重加密凭证表(工具随 v1.1 提供,
  此前手动执行)。

## 3. 恢复流程

```bash
# 1. 恢复数据库到新的 PostgreSQL 实例
pg_restore --clean --if-exists --dbname "$NEW_DATABASE_URL" mammoth-2026-09-08.dump

# 2. 以同一主密钥启动 mammoth(任何模式)
MAMMOTH_MASTER_KEY=<备份的密钥> \
MAMMOTH_DATABASE_URL="$NEW_DATABASE_URL" \
mammoth serve --mode=all

# 3. 自愈行为(无需人工干预):
#    - 启动时自动执行 schema 迁移(goose,幂等);
#    - 队列中未完成的消息按可见性超时重新投递;
#    - runner 崩溃期间的任务由 reaper 标记 interrupted,可通过 API 重试。
```

恢复演练:每季度在隔离环境执行一次上述流程,并验证:
1. 机器列表与标签完整;
2. 创建一个测试凭证并注册一台 fake 机器(`protocol: fake`)验证主密钥可解密;
3. 提交一个 power 任务并确认端到端成功。

## 4. 日常运营

| 任务 | 操作 |
|------|------|
| 健康检查 | `GET /healthz`(存活)、`GET /readyz`(就绪:DB+队列可达) |
| 指标 | `GET /metrics`(Prometheus:job/task 计数、BMC 延迟与错误、队列深度) |
| 事件审计 | `GET /api/v1/events?resource_type=...&type=...`(变更类事件即审计流) |
| 事件流 | `GET /api/v1/events/stream`(SSE,Last-Event-ID 断线续传) |
| 卡死任务 | 心跳超时后 reaper 自动标记 interrupted;`POST /jobs/{id}/tasks/{tid}/retry` 重试 |
| 死信 | 队列消息超过 `MAMMOTH_QUEUE_MAX_RECEIVE_COUNT` 次投递进入死信(不再投递);对应任务已终态失败,排障后显式重试 |
| 演示/评估 | 注册 `protocol: fake` 的机器即可全流程演练,无需真实 BMC |

## 4.4 API token 轮换

`MAMMOTH_API_TOKEN` 是静态 bearer token,无在线轮换。轮换 = 生成新 token
→ 更新 env → 重启进程(旧 token 随重启立即失效):

```bash
mammoth token generate
```

把输出写入 env 文件(或 systemd `EnvironmentFile=`、compose `env_file:`),
即 `MAMMOTH_API_TOKEN=<输出>`,然后重启进程,最后验证:

```bash
curl -H "Authorization: Bearer $MAMMOTH_API_TOKEN" http://localhost:8080/api/v1/machines
```

token 由部署方持有与备份,与数据库分域(同 `MAMMOTH_MASTER_KEY`);轮换无
宽限期,新旧客户端需在切换窗口内同步。

## 4.5 PXE 网络引导部署前提(M7)

`boot.strategy=pxe` 依赖机器面之外的一组网络服务(proxyDHCP / TFTP /
iPXE 脚本),默认关闭。启用清单:

1. **网络位置**:mammoth(netboot facet)驻留目标机器的装机 L2 时零配置
   ——proxyDHCP 靠广播工作,**同一 L2 只能有一个应答者**,与站点 DHCP 的
   共存是"跨主机"共存(proxyDHCP 只应答 PXEClient,站点 DHCP 继续拥有
   地址分配);**同机不可共存**:DHCP 服务的 UDP 67 是排他的,把 mammoth
   与站点 DHCP 放同一台机器会绑定失败(启用态下属 fatal,设计如此)。
   **跨网段(跨 VLAN)**:relay 的 ip helper 加一项指向 mammoth:67,
   mammoth 按 RFC 2131 §4.1 将应答发回 giaddr:67 由 relay 转回(单 L2
   不受影响);单播环节(NBP/脚本/kernel)路由可达即可。真机 relay 回归
   未完成,见 [11-pxe-walkthrough.md](11-pxe-walkthrough.md) §4;当前最稳
   拓扑是 mammoth 每个装机 VLAN 一条腿(服务监听全接口,天然多 L2)。
2. **端口与权限**:UDP 67(proxyDHCP)、4011(PXE boot-server discovery)、
   69(TFTP)、514(安装器日志 sink,`MAMMOTH_PXE_SYSLOG_PORT` 可改)是特权
   端口——容器部署需 `network_mode: host`(或 macvlan)+
   `CAP_NET_BIND_SERVICE`;裸机部署可用 `setcap cap_net_bind_service=+ep`。
   kernel/initrd 走 API 的 8080(机器面已需可达,无新增 TCP 端口)。NBP 之
   后的分发坚持 HTTP:国产系(麒麟/UOS 系)安装器 initrd 常达数百 MB
   (阵列卡/网卡固件全量塞入),TFTP 停等确认在体积与并发下必然超时;
   TFTP 仅承担 NBP(百 KB 级),属不可避免的最小面。
   **安装器日志 sink(UDP 514)**:d-i 经 `syslog=<mammoth 地址>` 内核参数
   把 ramfs 日志转发到 mammoth(ramfs 随安装器重启即逝,尸检别无它据);
   能反查到已武装任务的行带 `task_id` 落 task_logs(`jobs logs` 可查),
   外来客户端只记源 IP。514 被占(如站点 rsyslogd)仅告警降级,不阻塞
   PXE——日志是诊断,不是命脉。
3. **地址来源(二选一)**:
   - **站点 DHCP 存在** → 什么都不用配(proxy 模式:mammoth 只应答 PXEClient,
     地址分配仍归站点 DHCP);
   - **机房无 DHCP(常见)** → `MAMMOTH_PXE_DHCP_POOL`:mammoth 兼做全量
     DHCP(OFFER/ACK 租约,默认 /24 掩码、路由器默认取
     `MAMMOTH_PXE_NEXT_SERVER`,可用 `MAMMOTH_PXE_DHCP_ROUTER` 覆盖)。
     两种语法:**连字符范围**(`198.51.100.180-198.51.100.199` 或末段简写
     `198.51.100.180-199`)或**逗号独立地址列表**(`198.51.100.10,198.51.100.20`
     ——只租列出的这几个)。装机内核(dracut)的 DHCP 一并服务;非 PXE 的
     普通主机只给租约不给引导参数。租约内存态,覆盖引导+装机窗口足够。
     **有站点 DHCP 时勿开**,避免双 ACK。
4. **next-server 声明**:`MAMMOTH_PXE_NEXT_SERVER`(mammoth 在装机 L2 的 IPv4);
   `MAMMOTH_EXTERNAL_URL` 的 host 是 IP 字面量时自动派生,否则必填。
5. **零注册入门(可选)**:`MAMMOTH_PXE_ENROLL` + `MAMMOTH_PXE_ENROLL_TOKEN`
   (必配,缺失启动即败)。开启后未知 MAC 会拿到共享 enroll 探针
   (需 `MAMMOTH_PROBE_ALPINE_NETBOOT`;`MAMMOTH_PROBE_ALPINE_ISO` 可选,
   提供 apk 仓),/sys 扫描落 `pending_machines` 台账——见
   docs/05-inventory.md §4。威胁模型与任务引导树一致:token 在 overlay 与
   脚本内核参数里,拿到装机 L2 的人可伪造 pending 条目(低危:只污染台账,
   不触碰机器)。
6. **非 RHEL 家族的 PXE 安装源**:
   - **debian12**:载体为 d-i 官方 `netboot.tar.gz`
     (`MAMMOTH_PXE_DI_NETBOOT`,路径或 URL;ISO 自带 initrd 是 cdrom
     flavour,网络上不可用),安装源为 ISO 解包后的 HTTP 池(preseed
     mirror 指向池)——离线语义保持;
   - **ubuntu22**:载体与源均自动(casper 从 ISO 提取 + NFS squashfs 树,
     复用 nfsx 导出),无需额外配置;PXE 阶段仅 DHCP 网络;
   - 两条通路的 qemu 验证与真机回归待跑(见 roadmap M7 余项)。
   - **共享池树(内容寻址)**:安装源按镜像内容哈希共享
     (`MediaDir/pool-store/<sha256>/iso`),多任务复用一份解包(每棵
     ~2.5G,批量同发行版不再重复解包);经 `/netboot/store/<sha>/…`
     (HTTP)与 MediaDir NFS 导出(同路径)消费。树内是公开发行版内容
     (官方 ISO 解包 + mammoth 池签名公钥 deb),sha 即地址、不含任务敏感
     数据;首建原子落位(可见即完整),装机触碰会刷新活跃时间,启动时
     清扫闲置超 7 天的树。**更换池签名钥匙或 udeb 档案后需手工删除对应
     树**(首建时重做签名与补齐)。
7. **固件前提**:UEFI x64 的 Secure Boot **已支持**(shim+grubnet 链,
   shimx64.efi → grubx64.efi,来源见 assets/pxe/PROVENANCE.md);arm64 仍须
   关闭 Secure Boot。目标机 BIOS/UEFI 的 PXE/网络引导需在固件中可用。
8. **镜像形态**:distroless 主镜像已内嵌 iPXE 二进制(assets/pxe,来源与
   重建见 `assets/pxe/PROVENANCE.md`),无需额外包。
9. **运维**:引导项孤儿(进程崩溃残留)由 reaper 按任务终态清扫(默认
   1h);`MediaDir/netboot/<token>/` 引导树(kernel/initrd)随注销删除,
   共享池树 `MediaDir/pool-store/` 不随任务删除(启动时按 7 天闲置清扫,
   见上)。排查机器停在 PXE 提示符:先确认 `GET /api/v1` 的
   `netboot_enabled` 与任务事件的 `task.netboot_registered`,再抓 DHCP
   (DISCOVER 是否到达、OFFER 是否回出)。

### 4.5.1 外部 DHCP+TFTP 逃生门(MAMMOTH_PXE_MODE=external)

mammoth 不可能总当 PXE 服务:容器化部署拿不到特权 UDP、或站点网络归运维
团队管。external 模式把 mammoth 缩到纯 HTTP 面,UDP 侧(真 DHCP + TFTP)
交給站点 dnsmasq——**mammoth 零 UDP 绑定**,引导项注册、per-MAC 决策、
 enrollment、装机链路全部不变。

```sh
MAMMOTH_PXE_ENABLED=true
MAMMOTH_PXE_MODE=external        # builtin(默认)| external
# MAMMOTH_PXE_NEXT_SERVER 与 MAMMOTH_PXE_DHCP_POOL 在此模式下不需要(也不允许)
```

启动时 mammoth 把**静态 TFTP kit** 导出到 `MediaDir/netboot/external-tftp/`:

```
undionly.kpxe            # BIOS:PXE ROM → iPXE
shimx64.efi grubx64.efi  # UEFI x64 Secure Boot 链(Microsoft/Debian 签名)
grub/x86_64-efi/…        # grubnet 模块表
boot.ipxe                # iPXE 蹦床:chain .../netboot/script?mac=${net0/mac}
grub/grub.cfg            # grub 蹦床:configfile (http,mammoth)/netboot/grub/${net_default_mac}
dnsmasq.conf.example     # 站点 dnsmasq 配置模板(tag 路由已写好)
```

部署三步:①把 kit 目录同步给站点 TFTP(如 dnsmasq 的 tftp-root);②按
`dnsmasq.conf.example` 配站点 dnsmasq(真实地址池 + tag 路由:BIOS→
undionly→iPXE→蹦床,UEFI x64→shim→grub→蹦床,iPXE 类→boot.ipxe);
③蹦床里的 HTTP 地址已按 `MAMMOTH_EXTERNAL_URL` 填好,保证客户端可达即可。

> **站点 DHCP 为 Windows Server 的注意项**:Windows DHCP 的 option 43 需按
> TLV 十六进制串填写(值 `060 01 03 0A 04 00 <next-server-IP>` 形态,即
> PXEClient 子项 6/10),填成明文字符串客户端会静默忽略引导路径并报
> PXE-E32;option 66/67 在 Windows 界面里名称带前导零(066/067),值填
> mammoth 地址与 NBP 路径即可(多数 UEFI ROM 认 66/67,不强制 43)。

原理:**客户端自报身份**。mammoth 不参与 DHCP 就看不到 MAC,两个蹦床让
客户端把 MAC 放进 URL——iPXE 展开 `${net0/mac}`,grub 展开
`${net_default_mac}`——落回与 builtin 模式完全相同的 HTTP 端点
(`/netboot/script?mac=`、`/netboot/grub/<mac>`),entry 注册、enroll
回落、无 entry 即退出回盘的语义原样生效。**模式代价**:mammoth 看不到
DHCP,option 93 固件观测不工作(观测档案不更新);多 NIC 主机上 iPXE 的
`net0` 未必是 PXE 出口网卡,错位时查无 entry、机器回盘(蹦床注释已注明)。

```
undionly.kpxe            # BIOS:PXE ROM → iPXE
shimx64.efi grubx64.efi  # UEFI x64 Secure Boot 链(Microsoft/Debian 签名)
shimaa64.efi grubaa64.efi # UEFI aarch64 Secure Boot 链(同款签名对)
ipxe-amd64.efi           # 未签名 iPXE(UEFI x64):windows wimboot 载体的宿主
grub/x86_64-efi/…        # grubnet 模块表(x64)
grub/arm64-efi/…         # grubnet 模块表(arm64)
boot.ipxe                # iPXE 蹦床:chain .../netboot/script?mac=${net0/mac}
grub/grub.cfg            # grub 蹦床:configfile (http,mammoth)/netboot/grub/${net_default_mac}
dnsmasq.conf.example     # 站点 dnsmasq 配置模板(tag 路由已写好)
```

部署三步:①把 kit 目录同步给站点 TFTP(如 dnsmasq 的 tftp-root);②按
`dnsmasq.conf.example` 配站点 dnsmasq(真实地址池 + tag 路由:BIOS→
undionly→iPXE→蹦床,UEFI x64→shimx64→grubx64→蹦床,UEFI aarch64→
shimaa64→grubaa64→蹦床(client-arch 11),iPXE 类→boot.ipxe);
③蹦床里的 HTTP 地址已按 `MAMMOTH_EXTERNAL_URL` 填好,保证客户端可达即可。

原理:**客户端自报身份**。mammoth 不参与 DHCP 就看不到 MAC,两个蹦床让
客户端把 MAC 放进 URL——iPXE 展开 `${net0/mac}`,grub 展开
`${net_default_mac}`——落回与 builtin 模式完全相同的 HTTP 端点
(`/netboot/script?mac=`、`/netboot/grub/<mac>`),entry 注册、enroll
回落、无 entry 即退出回盘的语义原样生效。**模式代价**:mammoth 看不到
DHCP,option 93 固件观测不工作(观测档案不更新);多 NIC 主机上 iPXE 的
`net0` 未必是 PXE 出口网卡,错位时查无 entry、机器回盘(蹦床注释已注明)。

qemu 同型验证(网桥 + dnsmasq + UEFI guest 经此链完成 alpine agent 全装)
见 `scripts/pxe-dev/external-e2e.sh`。

**Windows 机器(wimboot 载体)的额外一步**:按 MAC 把目标机钉到未签名
iPXE——wimboot 只认 iPXE 宿主,且要求目标机 **Secure Boot 关闭**(边界与
机制见 docs/compat/distros.md §windows;builtin 模式下 proxyDHCP 对
wimboot entry 的 UEFI x64 客户端自动直发 `ipxe-amd64.efi`,无需手工):

```
dhcp-host=<win-machine-mac>,set:winboot
dhcp-boot=tag:winboot,tag:!ipxe,ipxe-amd64.efi,,<tftp-server>
```

windows 全链 rig 见 `scripts/windows-dev/external-win-e2e.sh`。

## 4.5.2 Windows PXE 的部署层 SMB 导出

windows wimboot 载体的 install 源是**部署层 SMB 只读共享**(与 NFS 介质
导出同哲学,引擎不内建 SMB 服务面——SMB 交 samba/Windows 文件共享):

```sh
MAMMOTH_WINDOWS_INSTALL_SMB_UNC='\\198.51.100.248\mammoth-media'  # 必填,UNC
MAMMOTH_WINDOWS_INSTALL_SMB_USER=''      # 可选;空 = guest 导出
MAMMOTH_WINDOWS_INSTALL_SMB_PASSWORD=''
```

share 指向介质仓库(`MAMMOTH_MEDIA_DIR`,WinPE 从
`<share>\pool-store\<sha256>\win\tree\sources\install.wim` 取安装源),
只读即可。samba 最小配置:`[mammoth-media] path=/data/mammoth/media +
guest ok = yes + read only = yes + map to guest = Bad User`(或固定凭据,
user/password 字符限 `[A-Za-z0-9._@-]`——cmd 批处理不可安全引用元字符)。
未配置时 windows PXE 提交即 `SCHEMA_WINDOWS_SMB_SHARE_REQUIRED`;
配置后能力位 `capabilities.windows_smb_share` 置真。

**arm64 注记**:aarch64 的 DHCP→TFTP→蹦床链与 x64 同构(opt 93=11→
shimaa64,与 MAAS/RFC 4578 一致),签名链(shimaa64→grubaa64 在 Secure
Boot 下)已经 qemu AAVMF 验证(`scripts/pxe-dev/arm64-sb-chain.sh`)。
但 aarch64 的 PXE 投递层在 qemu 上不可验——上游架构边界:ArmVirtQemu
固件没有 UEFI PXE 栈(SNP 仅 IA32/X64/EBC),arm64 引导链的端到端
DHCP/TFTP 回归需要 ARM 真机(如华为 TaiShan)窗口。发行版侧注意:
aarch64 没有 alpine extended ISO,agent 路径产品化时包池需另解。

## 4.6 HTTPS 终止(生产)

mammoth 自身监听 HTTP(distroless 内无证书管理),生产要求 HTTPS 在反向代理
终止(docs/security-baseline.md §2)。附带的 Caddy 样例把 TLS 终止在
mammoth 之前:

```bash
MAMMOTH_TLS_HOST=bmc.example.com \
docker compose -f deploy/compose.all-in-one.yml -f deploy/compose.tls.yml up -d
```

- `MAMMOTH_TLS_HOST` 填公网域名时,Caddy 自动申请 Let's Encrypt 证书
  (需 80 端口可达做 HTTP-01 校验);填 IP 字面量则用自签证书(仅测试);
- 生产应把 all-in-one 里 `8080:8080` 的端口映射移除或防火墙隔离,使
  API 仅经 Caddy 的 443 可达;
- 机器面 `/render/*`、`/netboot/*` 与 BMC 的 NFS 挂载不走 HTTPS(装机
  内核/anaconda 不校验 TLS),仍按 06-install-pipeline 的链路访问。

## 5. 升级

1. 备份数据库(§2);
2. 更换镜像/二进制;
3. 启动时迁移自动执行(goose;迁移文件只增不改——已应用的迁移不可修改,
   一切 schema 变更(含删列/改列)都以新增迁移文件的方式落地);
4. 回滚 = 回退镜像 + 恢复备份。schema 演进以**相邻版本读写兼容**为目标:
   新增列须 nullable 或带默认(老二进制 INSERT 不得撞上无默认的 NOT NULL
   新列);字段废弃两步走——先停代码读写,下一个迁移再物理删除——使
   "仅回退镜像、不动库"的快速回滚在相邻版本间始终可用。一步到位的删列/
   改列也可行,代价是该版本区间内回滚只能恢复备份;跨多个迁移的回滚
   一律需要恢复备份。
