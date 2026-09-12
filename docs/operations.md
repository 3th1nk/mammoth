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

## 5. 升级

1. 备份数据库(§2);
2. 更换镜像/二进制;
3. 启动时迁移自动执行(goose,仅追加式演进);
4. 回滚 = 回退镜像 + 恢复备份(迁移仅新增,老二进制可读新库是设计目标,
   但跨多个 major 迁移的回滚仍需恢复备份)。
