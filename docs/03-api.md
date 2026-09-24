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
GET    /api/v1                                  # capabilities:版本/资源/发行版支持矩阵
GET    /api/v1/config                           # 生效配置只读快照(env 名为键,denylist 脱敏:配置过="***",未配置=null;无写路径)

POST   /api/v1/credentials
GET    /api/v1/credentials                      # 元数据列表(无 secret,供选择器)
GET    /api/v1/credentials/{id}
DELETE /api/v1/credentials/{id}

POST   /api/v1/machines
GET    /api/v1/machines?state=ready&labels=rack=A3&q=SN-QA&cursor=&page_size=50
GET    /api/v1/machines/{id}
PATCH  /api/v1/machines/{id}
DELETE /api/v1/machines/{id}
GET    /api/v1/machines/{id}/current-tasks      # 未完结任务视图(装 busy 徽章/重装预检)
GET    /api/v1/machines/{id}/layout
GET    /api/v1/machines/{id}/drives             # 控制器实时物理盘表(带外同步读)
GET    /api/v1/machines/{id}/bios               # 控制器实时 BIOS 属性表(带外同步读)
GET    /api/v1/machines/{id}/health             # 控制器实时健康快照:传感器 + 电源态 + overall(带外同步读)
GET    /api/v1/machines/{id}/sel                # 控制器系统事件日志,倒序截断 500 条(带外同步读)
GET    /api/v1/machines/{id}/console            # 一次性 KVM URL
POST   /api/v1/machines/batch-labels            # 批量打标签(单事务全有或全无;remove 按 key 删 + add upsert)
POST   /api/v1/machines/{id}/install-plan       # 试算(只读;已实现 V1,见 §4)
POST   /api/v1/machines/{id}/actions            # 单机动作 → 202 + job

POST   /api/v1/images                           # 工件库注册 + 触发拉取 → 202
GET    /api/v1/images                           # 注册清单(新→旧)
GET    /api/v1/images/{id}                      # 注册详情(fetching/ready/failed)
DELETE /api/v1/images/{id}                      # 删注册(无共享者时回收缓存文件)

POST   /api/v1/jobs                             # → 202
GET    /api/v1/jobs/{id}
POST   /api/v1/jobs/{id}/cancel
GET    /api/v1/jobs/{id}/tasks?state=failed
GET    /api/v1/jobs/{id}/tasks/{tid}
GET    /api/v1/jobs/{id}/tasks/{tid}/logs       # 任务执行日志(日志双写落库半边)
GET    /api/v1/jobs/{id}/tasks/{tid}/logs/stream # 日志 SSE(回放+尾随,终态发 eos 收流)
POST   /api/v1/jobs/{id}/tasks/{tid}/retry
GET    /api/v1/jobs/{id}/events                 # job 级 SSE

GET    /api/v1/events?resource=job_x9k2         # 事件/审计查询(游标分页)
GET    /api/v1/events/stream                    # 全局 SSE(Last-Event-ID 断线续传)

POST   /api/v1/webhooks                         # 订阅(HMAC-SHA256 签名投递)
GET    /api/v1/webhooks/{id}
DELETE /api/v1/webhooks/{id}
```

> images / templates 资源未进入实现(契约中亦不存在);spec 模板化复用由
> 客户端自行管理,引擎只收最终 Install Spec。

### 单机动作

统一入口 `POST /machines/{id}/actions`,以 `type` 区分:

```jsonc
{"type": "discover",       "probe": "auto"}     // auto | redfish | inband_ssh | ramdisk
                                                // ramdisk 可选,启用清单见 docs/05-inventory.md §4;
                                                // 另接受 "boot":"pxe"|"virtual_media" 选 ramdisk 载体
                                                // (缺省随部署 boot 策略缺省)
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
| 过滤/排序 | `?state=failed&labels=env=prod`;机器列表另有模糊搜索 `?q=`(对机器 ID、BMC 地址、序列号、厂商、型号、label 键值做大小写不敏感子串匹配,LIKE 元字符按字面处理,与其它过滤条件 AND 组合);`?order_by=created_at&order=desc`(游标键随 order_by 走) |
| 时间 | RFC 3339 UTC;时长字段以 `_seconds` / `_duration_ms` 后缀显式标注单位 |
| 凭证 | 只写;支持请求内联(服务端转存为 credential 资源并返回引用) |
| 追踪 | 每响应 `X-Request-Id`;task 日志与事件全链路携带 `task_id` |
| 事件 | SSE(`text/event-stream`);事件类型:`task.stage_changed` `task.state_changed` `job.state_changed` `machine.discovered` `pending.sighted`(零注册首见) `pending.reported`(探针报告落库)等;流空闲期以 `: keepalive` 注释帧保活,任务日志流在终态回放完后发 `eos` 收流 |
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

### install-plan 试算(已实现 V1)

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

### 客户端接入方式(契约即 SDK)

集成面不提供手写 SDK:OpenAPI 3.1 契约(`api/openapi.yaml`,亦随服务端
二进制经 embedded-spec 分发,`GET /api/v1` 自描述)本身就是对任何语言
集成方的承诺——手写封装等于把契约复制第二份,随契约演进必然漂移。
各语言的接入姿势:

- **任意语言**:用各自生态的 OpenAPI 工具链从契约生成客户端
  (openapi-generator / oapi-codegen / datamodel-code-generator 等),
  或直接 curl——`scripts/acceptance.py` 与 `internal/cli`(纯 HTTP
  客户端,契约之外零语义)即是两个 in-repo 的手卷先例。
- **Go**:一条命令生成类型安全的客户端:

  ```sh
  go tool oapi-codegen -generate types,client \
    -package mammothclient -o client/api.go api/openapi.yaml
  ```

- **机器面例外**:`/render/{token}/...` 与 `/netboot/...` 由安装器/探针
  消费(shell 脚本),以路径中的 task token 为凭证,不进 SDK 语义。

生成式 client 的 pkg/ 化(输出进 `pkg/api/client` 供外部 import)**不预
做**:在出现第一个仓库外 Go 消费方(或仓库内第二个 Go HTTP 消费方)之前,
生成代码留在 `internal/`,避免为想象中的读者发行公共面;届时是 cfg.yaml
加一行输出的动作,见 docs/09-roadmap.md 等条件组。

### 破坏性动作确认(规划)

BMC 写动作(power / boot override / 虚拟介质)与未来的固件、擦盘类动作,
按风险分级引入**两段式确认契约**:请求体携带显式确认标志(如
`"confirm": true`),服务端对高危动作二次校验标志存在且匹配目标——仅靠
调用方封装层默认传递不算确认;是否要求确认做成**策略开关**(默认关闭,
面向全自动化调用方),开启时缺标志的动作以 4xx 拒绝。已内化的边界不因
开关改变:boot override 仅 next-boot 一次性、动作不隐式级联重启。

## 5. 契约演进规则

1. 新增字段不破坏契约;字段只能新增、废弃(标记 `deprecated`),不允许语义变更;
2. 枚举值新增视为兼容;枚举值删除视为破坏性变更;
3. 每个 release 生成契约 diff 报告,随版本发布;
4. `X-Mammoth-API-Version` 响应头返回服务端契约版本,便于客户端协商。
