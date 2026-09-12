# 08 · 内部数据模型

> 本文档描述服务内部的持久化模型(PostgreSQL)。字段仅覆盖核心语义,
> 完整 DDL 以迁移文件为准。命名遵循:`<resource>` 复数表名,snake_case。

## 1. ER 概览

```
credentials 1 ──── n machines 1 ──── n layout_snapshots
                  │
jobs 1 ──── n tasks 1 ──── n task_stages
                    │
                    └── events / task_logs
```

## 2. 表定义

### credentials

| 字段 | 类型 | 说明 |
|------|------|------|
| id | text PK | `cred_` 前缀,服务端生成 |
| name | text | 展示名,唯一约束 |
| type | text | `bmc` \| `ssh` |
| secret_encrypted | bytea | 静态加密(主密钥由部署方注入);**永不出库回显** |
| created_at / updated_at | timestamptz | |

### machines

| 字段 | 类型 | 说明 |
|------|------|------|
| id | text PK | `mch_` 前缀 |
| bmc_address | text | 唯一约束(同一带外地址只注册一次) |
| bmc_protocol | text | `redfish` \| `ipmi` \| `auto` |
| bmc_credential_id | text FK → credentials | |
| ssh_credential_id | text FK,nullable | 带内盘查用 |
| ssh_address | text,nullable | 带内寻址(契约 `machine.ssh.address`;迁移 00002,`inband_ssh` 需与凭证同时配置) |
| vendor / model / serial_number / firmware | text,nullable | 盘查回填 |
| hardware | jsonb,nullable | 盘查结果(hardware view) |
| power_state | text | `on` \| `off` \| `unknown` |
| state | text | `registering` \| `discovering` \| `ready` \| `error` |
| labels | jsonb | GIN 索引,支持 `?labels=k=v` 过滤 |
| last_error | jsonb,nullable | |
| created_at / updated_at | timestamptz | |

索引:`uk(machines.bmc_address)`;`gin(machines.labels)`。

### layout_snapshots

| 字段 | 类型 | 说明 |
|------|------|------|
| id | bigserial PK | 内部序号 |
| machine_id | text FK | `idx(machine_id, captured_at)` |
| captured_at | timestamptz | |
| source | text | `inband_ssh` \| `ramdisk` |
| content | jsonb | [04 §3](04-install-spec.md) 的 layout 结构 |

**只追加,不更新**;`machine` 视图取 `captured_at` 最新版本。
保留策略:每机保留最近 N 版(默认 10),过期清理由后台任务执行。

### images / templates(未实现,规划保留)

| 表 | 关键字段 |
|----|---------|
| images | id(`img_`), name, source, checksum, distro, size_bytes |
| templates | id(`tpl_`), name, spec(jsonb,即 Install Spec), labels |

> 现实现中介质引用直接进 Install Spec 的 `image.source`,spec 模板化复用由客户端管理。

### jobs

| 字段 | 类型 | 说明 |
|------|------|------|
| id | text PK | `job_` 前缀 |
| type | text | `install` \| `power` \| `discover` |
| request | jsonb | 原始请求快照(幂等重放的依据) |
| spec_resolved | jsonb,nullable | 合并 template/override 后的最终 spec(审计用) |
| policy | jsonb | concurrency / on_task_failure / verify_layout / timeout |
| idempotency_key | text,nullable | 唯一约束(24h 窗口内重放返回原结果) |
| state | text | `pending` \| `running` \| `succeeded` \| `partial` \| `failed` \| `canceled` |
| summary | jsonb | 各 state 计数(物化,列表页免聚合) |
| created_by | text | API token 标识 |
| created_at / finished_at | timestamptz | |

索引:`uk(jobs.idempotency_key)`;`idx(jobs.state, created_at)`。

### tasks

| 字段 | 类型 | 说明 |
|------|------|------|
| id | text PK | `tsk_` 前缀 |
| job_id | text FK | `idx(job_id)` |
| machine_id | text FK | |
| state | text | `pending` \| `running` \| `succeeded` \| `failed` \| `canceled` \| `interrupted` |
| flow_name | text | 状态机定义名:`install` \| `power` \| `discover`(stage 序列见 provision flow 定义) |
| stage_index | int | 当前/最后到达的 stage |
| stage_attempt | int | |
| stage_deadline | timestamptz,nullable | 当前 stage 的超时点 |
| heartbeat_at | timestamptz,nullable | runner 心跳 |
| owner_runner | text,nullable | 持有者(runner 实例标识) |
| context | jsonb | **结构化**任务上下文(schema 版本化) |
| error | jsonb,nullable | code / message / retryable |
| created_at / updated_at / finished_at | timestamptz | |

索引:`idx(tasks.job_id, state)`;`idx(tasks.heartbeat_at)`(reaper 扫描)。

**context 更新规则**:单条 UPDATE 带乐观锁
(`WHERE id=? AND stage_index=?`),禁止读-改-写全量回写。

### task_stages

| 字段 | 类型 | 说明 |
|------|------|------|
| task_id | text FK | `idx(task_id, seq)` |
| seq | int | |
| name | text | |
| state | text | |
| attempt | int | |
| started_at / finished_at | timestamptz | |
| duration_ms | int | 物化,阶段耗时直方图的数据源 |

### task_logs / events

| 表 | 关键字段 | 说明 |
|----|---------|------|
| task_logs | id(bigserial), task_id, ts, level, stage, message, attrs jsonb | ✅ 已建(M6):日志双写的落库半边(docs/02 §5.2)——携带 `task_id` 的结构化日志行在写入时同步落库,`GET /jobs/{id}/tasks/{taskId}/logs` 按游标检索;`idx(task_id, id)`;TTL 由 reaper 过期(`MAMMOTH_TASK_LOGS_TTL`,默认 90d),单表,月度分区留作超量后的演进 |
| events | id(bigserial), resource_type, resource_id, type, payload jsonb, ts | ✅ 已建;SSE 水位线按 id(1s 轮询);审计/事件查询 API 与 Webhook 投递的数据源 |

## 3. 一致性与并发的三条铁律

1. **任务状态只有 runner 推进**:控制面只允许写 `canceled`(cancel 请求),其余迁移
   全部经队列由 runner 执行——杜绝双写竞态;
2. **快照不可变**:layout 快照只追加;安装 spec 解析时绑定具体版本;
3. **心跳即所有权**:`owner_runner + heartbeat_at` 双字段判定任务归属,
   reaper 仅在心跳超时后转移(`interrupted`),原 runner 恢复后因乐观锁无法继续推进。
