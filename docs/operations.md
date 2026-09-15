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

1. **网络位置**:mammoth(netboot facet)须驻留目标机器的装机 L2——
   proxyDHCP 靠广播工作。**同一 L2 只能有一个应答者**,与站点 DHCP 的共存
   是"跨主机"共存(proxyDHCP 只应答 PXEClient,站点 DHCP 继续拥有地址
   分配);**同机不可共存**:DHCP 服务的 UDP 67 是排他的,把 mammoth 与
   站点 DHCP 放同一台机器会绑定失败(启用态下属 fatal,设计如此)。
2. **端口与权限**:UDP 67(proxyDHCP)、4011(PXE boot-server discovery)、
   69(TFTP)是特权端口——容器部署需 `network_mode: host`(或 macvlan)+
   `CAP_NET_BIND_SERVICE`;裸机部署可用 `setcap cap_net_bind_service=+ep`。
   kernel/initrd 走 API 的 8080(机器面已需可达,无新增 TCP 端口)。NBP 之
   后的分发坚持 HTTP:国产系(麒麟/UOS 系)安装器 initrd 常达数百 MB
   (阵列卡/网卡固件全量塞入),TFTP 停等确认在体积与并发下必然超时;
   TFTP 仅承担 NBP(百 KB 级),属不可避免的最小面。
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
5. **固件前提**:UEFI x64 的 Secure Boot **已支持**(shim+grubnet 链,
   shimx64.efi → grubx64.efi,来源见 assets/pxe/PROVENANCE.md);arm64 仍须
   关闭 Secure Boot。目标机 BIOS/UEFI 的 PXE/网络引导需在固件中可用。
6. **镜像形态**:distroless 主镜像已内嵌 iPXE 二进制(assets/pxe,来源与
   重建见 `assets/pxe/PROVENANCE.md`),无需额外包。
7. **运维**:引导项孤儿(进程崩溃残留)由 reaper 按任务终态清扫(默认
   1h);`MediaDir/netboot/<token>/` 引导树随注销删除。排查机器停在
   PXE 提示符:先确认 `GET /api/v1` 的 `netboot_enabled` 与任务事件的
   `task.netboot_registered`,再抓 DHCP(DISCOVER 是否到达、OFFER 是否
   回出)。

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
3. 启动时迁移自动执行(goose,仅追加式演进);
4. 回滚 = 回退镜像 + 恢复备份(迁移仅新增,老二进制可读新库是设计目标,
   但跨多个 major 迁移的回滚仍需恢复备份)。
