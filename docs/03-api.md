# 03 · API 设计规范

> 设计基准:Google AIP(资源与长任务语义)、RFC 9457(错误结构)、Stripe(凭证与幂等)、
> OpenAPI 3.1(契约优先)。**契约文件是唯一事实源**,本文是其人读版。

## 1. 资源模型

```
credential ──▶ machine ──▶ layout(快照)          资产域
image / template ──▶ job ──▶ task ──▶ events      编排域
```

| 资源 | 职责 |
|------|------|
| `credential` | BMC / SSH 凭证。**只写不可读**:响应永不包含 secret |
| `machine` | 裸机资产:带外寻址、硬件规格、分区快照引用、电源态 |
| `image` | 注册的安装镜像(source/checksum/distro),安装时也可内联直链 |
| `template` | 可复用的 Install Spec;job 内可引用 + 覆盖 |
| `job` | 一切异步操作的载体(install / power / discover),含批量 |
| `task` | job 内 per-machine 子任务,只读,含阶段明细 |
| `event` | 任务事件,经 SSE 流式推送 |

资源 ID 为带前缀的不透明字符串:`mch_` `cred_` `img_` `tpl_` `job_` `tsk_`。
对外永不暴露内部自增主键。

## 2. URL 空间

```
POST   /api/v1/credentials
GET    /api/v1/credentials/{id}
DELETE /api/v1/credentials/{id}

POST   /api/v1/machines
GET    /api/v1/machines?state=ready&labels=rack=A3&cursor=&page_size=50
GET    /api/v1/machines/{id}
PATCH  /api/v1/machines/{id}
DELETE /api/v1/machines/{id}
GET    /api/v1/machines/{id}/layout
GET    /api/v1/machines/{id}/console            # 一次性 KVM URL
POST   /api/v1/machines/{id}/actions            # 单机动作 → 202 + job

GET    /api/v1/images        POST / DELETE /{id}
GET    /api/v1/templates     POST / DELETE /{id}

POST   /api/v1/jobs                             # → 202
GET    /api/v1/jobs/{id}
POST   /api/v1/jobs/{id}/cancel
GET    /api/v1/jobs/{id}/tasks?state=failed
GET    /api/v1/jobs/{id}/tasks/{tid}
POST   /api/v1/jobs/{id}/tasks/{tid}/retry
GET    /api/v1/jobs/{id}/events                 # SSE

GET    /api/v1/events?resource=job_x9k2         # 全局 SSE
```

### 单机动作

统一入口 `POST /machines/{id}/actions`,以 `type` 区分:

```jsonc
{"type": "discover",       "probe": "auto"}     // auto | redfish | inband_ssh
{"type": "power_on"}
{"type": "power_off"}                            // 硬关机
{"type": "soft_off"}
{"type": "reboot"}                               // 软重启
{"type": "hard_reboot"}
{"type": "cycle"}                                // 硬关→开
{"type": "set_boot_device", "device": "pxe", "once": true}   // pxe|disk|cdrom|bios
{"type": "mount_media", "image_url": "...", "eject_after": false}
```

**设计决策:所有 BMC 动作返回 202 + job,不提供同步 BMC 调用端点。**
不同厂商 BMC 响应时间相差一个数量级,同步端点必然出现超时灰色地带;
统一异步模型换来单一的状态查询、取消与可观测语义,对批量场景是必需项。

## 3. job / task 状态机

```
job:   pending → running → succeeded | partial | failed | canceled
task:  pending → running → succeeded | failed | canceled
                        │(心跳丢失,由 reaper 判定)
                        └→ interrupted(可重试)
```

`GET /jobs/{id}`:

```jsonc
{
  "id": "job_x9k2",
  "type": "install",
  "state": "running",
  "summary": { "total": 50, "pending": 5, "running": 10, "succeeded": 32, "failed": 3 },
  "created_at": "2026-09-07T08:00:00Z",
  "finished_at": null
}
```

`GET /jobs/{id}/tasks/{tid}`:

```jsonc
{
  "machine_id": "mch_7f3a9c",
  "state": "failed",
  "attempt": 2,
  "stages": [
    { "name": "verify_layout", "state": "succeeded", "duration_ms": 1200 },
    { "name": "prepare_media", "state": "succeeded", "duration_ms": 8400 },
    { "name": "install_os",    "state": "failed",    "duration_ms": 2140000 }
  ],
  "error": { "code": "INSTALL_TIMEOUT", "message": "...", "retryable": true }
}
```

`stages` 是可观测性的核心:阶段粒度的状态与耗时,使"慢在哪一步"无需查日志即可回答。

## 4. 横切规范

| 项 | 规范 |
|----|------|
| 版本 | 路径版本 `/api/v1`;破坏性变更发 v2,不搞静默升级 |
| 错误 | RFC 9457 `application/problem+json`,扩展 `code`(机器可读错误码,注册表维护)与 `retryable` |
| 幂等 | `Idempotency-Key` header 适用于所有创建类 POST;24h 窗口内重放返回原结果 |
| 分页 | cursor 制:`?page_size=&cursor=`,响应含 `next_cursor`;不提供 offset 深翻页 |
| 过滤/排序 | `?state=failed&labels=env=prod`;`?order_by=created_at&order=desc` |
| 时间 | RFC 3339 UTC;时长字段以 `_seconds` / `_duration_ms` 后缀显式标注单位 |
| 凭证 | 只写;支持请求内联(服务端转存为 credential 资源并返回引用) |
| 追踪 | 每响应 `X-Request-Id`;task 日志与事件全链路携带 `task_id` |
| 事件 | SSE(`text/event-stream`);事件类型:`task.stage_changed` `task.state_changed` `job.state_changed` `machine.discovered` 等 |
| 契约 | OpenAPI 3.1;SDK 由 spec 生成(Go/Python/TypeScript);`GET /api/v1` 返回能力自描述 |
| 鉴权 | Bearer token;首版内置静态 token 管理,预留外部 IdP 适配点 |

### 错误响应示例

```jsonc
HTTP/1.1 409 Conflict
Content-Type: application/problem+json

{
  "type": "https://mammoth.dev/errors/layout-drift",
  "title": "Layout drift detected",
  "status": 409,
  "detail": "partition table of /dev/nvme1n1 changed since snapshot 2026-09-07T08:00:00Z",
  "instance": "/api/v1/jobs/job_x9k2/tasks/tsk_22",
  "code": "LAYOUT_DRIFT",
  "retryable": false
}
```

### 错误码命名规则

`<域>_<现象>`,域取值:`SCHEMA` `CREDENTIAL` `BMC` `LAYOUT` `MEDIA` `INSTALL` `NETWORK` `JOB`。
示例:`SCHEMA_INVALID_STORAGE`、`BMC_UNREACHABLE`、`BMC_AUTH_FAILED`、
`LAYOUT_DRIFT`、`LAYOUT_DISK_NOT_FOUND`、`INSTALL_TIMEOUT`、`MEDIA_MOUNT_FAILED`。

### install-plan 试算(提案,未实现)

业务层(UI/编排方)的典型交互是:**采集机器信息 → 展示给用户 → 用户配置
Install Spec → 确认后安装**。当前 spec 校验只发生在 `POST /jobs` 提交时
(校验失败靠任务预失败试错),缺一个**只读试算端点**让业务层在提交前拿到
"这份 spec 在这台机器上会装成什么样":

```
POST /machines/{id}/install-plan      (body = InstallSpec,同 install job)
→ 200  安装计划(仅解析,不入队、不装机):
   {
     "resolved_disks": [ { "device": "sda",
                           "matched_by": {"protocol": "raid"},
                           "size_bytes": 3999999721472,
                           "planned_partitions": [
                             {"number":1,"size":"512M","fs":"vfat",
                              "mount":"/boot/efi","flags":["esp"]} ] } ],
     "resolved_network": [...],
     "boot_drive": "sda",
     "driver_notes": [ "netcfg: single interface only" ],
     "warnings": [ ... ]
   }
→ 422  与真实提交同源的 SCHEMA_* / LAYOUT_* 错误(不产生任务)
```

实现要点:复用 verify_layout 与驱动渲染校验的解析逻辑(纯函数式,只读);
带内快照缺失时 `resolved_*` 降级为带外视角并附 `warnings`(视角一致性——
Redfish 卷名/serial 与安装器视图可能不一致,见 docs/compat/huawei.md);
驱动方言限制(如 netcfg 单接口)以 `driver_notes` 结构化返回,供展示层
在配置阶段就拦住,而不是装到一半 RENDER_FAILED。

## 5. 契约演进规则

1. 新增字段不破坏契约;字段只能新增、废弃(标记 `deprecated`),不允许语义变更;
2. 枚举值新增视为兼容;枚举值删除视为破坏性变更;
3. 每个 release 生成契约 diff 报告,随版本发布;
4. `X-Mammoth-API-Version` 响应头返回服务端契约版本,便于客户端协商。
