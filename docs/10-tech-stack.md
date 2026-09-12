# 10 · 技术栈与选型决策

> 技术选型的第一原则:**依赖树最短**。运行时强依赖只有 PostgreSQL 一个,
> 外部工具只有 xorriso 一个。这一原则服务于两个目标:开源项目的最低上手门槛,
> 以及受限环境(私有化/边缘)的可部署性。

## 1. 选型总表

| 层 | 选型 | 版本基线 | 用途 |
|----|------|---------|------|
| 主语言 | Go | ≥1.24 | 全部服务端与 CLI |
| API 框架 | Gin + oapi-codegen(gin 生成模式) | gin ≥1.9 | 契约优先:OpenAPI 3.1 生成 gin 路由与强类型 handler 绑定,业务实现在手写层 |
| 主存储 | PostgreSQL | ≥14 | 唯一权威存储:资源、契约、任务状态、事件 |
| 任务队列 | 数据库表队列(`SELECT … FOR UPDATE SKIP LOCKED`) | — | job/task 指令分发;状态与消息同库 |
| 分布式协调 | PG advisory lock | — | runner 抢占、调度互斥 |
| 迁移 | goose | — | SQL 文件式 schema 迁移 |
| Redfish 客户端 | gofish(`stmcginnis/gofish`) | — | 带外盘查与控制;OEM 扩展自行封装 |
| IPMI | 纯 Go 协议实现(以 `vmware/goipmi` 为基础裁剪) | — | chassis/bootdev/FRU 常用子集;零外部依赖 |
| SSH | `golang.org/x/crypto/ssh` | — | inband_ssh 探针、介质中转 |
| 内置 NFS 导出 | go-nfs(用户态 NFSv3)+ 自带 mini rpcbind(`internal/nfsx`) | — | 介质目录的进程内只读导出,BMC 直挂 `nfs://<mammoth>/`;大规模部署关掉(`MAMMOTH_NFS_EXPORT=false`)走外部 NFS |
| ISO 重打包 | xorriso(探测/提取/装配,纯用户态) | — | 发行版 ISO → `boot-<token>.iso`(应答文件与内核参数烘入) |
| 模板渲染 | 标准库 `text/template` | — | 应答文件渲染(纯文本注入) |
| 介质组装 | xorriso | 外部工具,仅 builder 面引用 | 通用引导介质制作 |
| 日志 | `log/slog` | — | 结构化 JSON 日志 |
| 指标 | Prometheus `client_golang` | — | /metrics |
| 追踪 | OpenTelemetry API(边界埋点,SDK no-op 默认) | — | 四类边界埋点自 M0 存在;OTLP 导出按配置启用 |
| CLI | cobra + goreleaser | — | mammoth CLI 与发布 |
| 测试 | 标准库 testing + 录制的 Redfish 响应(契约测试) | — | BMC 驱动 fake/simulator 是可测性关键 |

## 2. 关键决策记录

### D1 · Go(而非 Rust/Python)

- 领域事实标准:Tinkerbell、Metal3 等新一代裸机引擎均为 Go,生态可对照、贡献者可迁移;
- 单二进制静态编译,对"客户环境不可控"的部署形态是决定性优势;
- 工作负载是 IO 密集(大量 BMC 轮询、SSH 探针、事件流),goroutine 模型精确匹配,
  Rust 的性能与内存安全优势在此场景无边际收益,却显著抬高贡献门槛。

### D2 · PostgreSQL(而非 MySQL / SQLite)

- `jsonb` 是硬需求:Install Spec、task context、labels 均为半结构化数据,
  labels 过滤依赖 GIN 索引,spec 校验依赖 jsonpath;
- `SELECT … FOR UPDATE SKIP LOCKED` 使表队列成为一等公民(见 D3);
- 开源基础设施圈的默认心智(GitLab 等同款),文档与社区支持成本低;
- SQLite 保留为 M6 的最小部署形态(单机演示/评估),不作为集群控制面选项。

### D3 · 表队列(而非 Redis / NATS),队列语义独立封装

- 少一个运行时强依赖,部署单元从"PG + Redis"降为"PG 一个";
- 任务状态与队列消息**同库同事务**:领取任务、推进状态、写事件在单个事务内完成,
  从机制上消除"消息已消费但状态未落库"类的双写不一致;
- 吞吐不是瓶颈:BMC 操作与安装的节奏以秒/分钟计,每秒数十条指令的队列负载
  远低于 PG 表队列的能力上限。

**队列抽象层(TaskQueue)**是明确的设计目标,语义按"任务指令队列"的最小公共集定义
(SQS 同款语义),三个实现按此适配:

```go
type TaskQueue interface {
    Enqueue(ctx context.Context, queue string, payload []byte, opts EnqueueOptions) error
    // 领取一条消息并获得回执;visibilityTimeout 内未 Ack 则自动重回队列(至少一次投递)
    Dequeue(ctx context.Context, queue string, visibilityTimeout time.Duration) (Receipt, error)
    Ack(ctx context.Context, receipt Receipt) error              // 完成确认
    Nack(ctx context.Context, receipt Receipt, reason error) error // 重回队列或转入死信(超阈值)
    ExtendVisibility(ctx context.Context, receipt Receipt, d time.Duration) error // 心跳续期
}
```

实现路线与适配度:

| 实现 | 定位 | 适配说明 |
|------|------|---------|
| PostgreSQL(SKIP LOCKED) | **默认**,生产 | 领取/Ack/续期与任务状态同事务;lease 即可见性超时 |
| SQLite | **测试与最小部署** | 同一表队列 SQL 方言子集;单元测试不依赖外部服务 |
| RabbitMQ(AMQP) | 未来可选 | 语义映射良好:prefetch ≈ 可见性、ack/nack 原生、死信队列原生 |
| Kafka | **不适合任务队列** | 无 per-message ack 与可见性语义,强行适配需自建 offset 管理;Kafka 的正确位置是**事件流**(events 对外投递),不是任务指令 |

抽象边界纪律:接口不暴露任何实现特有概念(路由键、topic 分区、交换机拓扑),
上层只见 queue 名与消息字节;实现差异(死信策略、重试计数)收在各实现的
`Nack` 行为与配置中。

### D4 · Gin + oapi-codegen(gin 模式)

- 本项目承诺"OpenAPI 契约是唯一事实源"([03-api.md](03-api.md) §5),
  oapi-codegen 的 gin 生成模式与此同向:路由注册与请求/响应类型全部由契约生成,
  业务逻辑在手写 handler 层,契约漂移在编译期暴露;
- Gin 提供成熟的中间件生态(恢复、访问日志、限流),按约定组合而非自研;
- 分层纪律:生成的代码(generated/)与手写业务(handler/)物理隔离,
  重生成不覆盖手写层。

### D5 · IPMI 纯 Go 实现(而非封装 ipmitool)

- ipmitool 是万能但引入系统包依赖与 shell 边界,破坏"单二进制"承诺;
- Mammoth 所需 IPMI 子集很窄(chassis power/bootdev、FRU、SEL 读取),
  纯 Go 实现可控可测;
- 厂商 OEM 命令(如私有虚拟介质指令)以驱动层可选接口渐进补充,
  无法覆盖的机型由兼容矩阵声明降级路径(Redfish 优先策略下,IPMI-only 机型本身已是少数)。

### D6 · 标准库日志/模板 + 可观测埋点先行

- slog / text/template 为标准库能力,零依赖、零学习成本、永不弃坑;
- **可观测埋点自 M0 写入代码**,而非事后补:裸机安装是跨 api/builder/runner/BMC 的
  长链路异步任务,链路关联(task_id 贯穿)必须在第一天就建立;
- 分层策略(后端可插拔,默认零外部依赖):
  - **日志**:slog 结构化,标准字段集
    (`task_id` `machine_id` `job_id` `request_id` `stage`)全链路透传;
  - **指标**:Prometheus 自 M0 暴露,核心清单:job/task 计数(按 state)、
    stage 耗时直方图、`bmc_request_duration_seconds`(按 vendor/操作/结果分类)、
    `bmc_errors_total`(按错误码)、队列深度;
  - **追踪**:OTel **API** 埋点在四类边界(HTTP handler、队列消费、BMC 调用、渲染),
    默认 no-op exporter(零开销、零依赖);配置启用 OTLP 导出后才引入 SDK 传输依赖;
  - trace 关联:task 的 span 上下文随队列消息载荷透传,跨进程面不断链。

## 3. 依赖树

```
运行时(强依赖)
├── PostgreSQL ≥14          # 唯一外部服务依赖
├── xorriso                 # 仅 builder 部署单元(外部工具)
└── 目标机 BMC              # Redfish HTTP / IPMI UDP,标准协议

运行时(可选)
├── S3 兼容对象存储          # 介质仓库的可选后端(默认本地卷)
└── OTel Collector          # 可选追踪

构建期
├── Go toolchain
├── oapi-codegen / goose / goreleaser / cobra / gin
└── 录制的 Redfish 响应样本(docs/compat/ 样本库)
```

## 4. 仓库布局约定

```
mammoth/
├── api/openapi.yaml          # 契约:唯一事实源
├── cmd/mammoth/              # 入口:serve --mode / CLI 子命令(cobra)
├── internal/
│   ├── api/                  # 控制面:HTTP 实现(契约生成绑定 + 手写 handler、SSE、webhook 投递面)
│   ├── bmc/                  # BMC 驱动:redfish / ipmi / fake + 可选能力(VolumeCreator 等)
│   ├── provision/            # 状态机与 flow 定义(六阶段 install 流水线)、verify_ready
│   ├── builder/              # 介质组装:发行版 ISO 探测/重打包(boot-<token>.iso)
│   ├── inventory/            # 探针:inband_ssh(redfish 盘查在 bmc;ramdisk 规划)
│   ├── render/               # 应答文件渲染,按方言分包:kickstart / autoinstall / preseed
│   ├── store/                # repo + PG 迁移 + 表队列(PG/SQLite 双实现)
│   ├── nfsx/                 # 内置只读 NFSv3 导出(go-nfs + mini rpcbind)
│   ├── mediarelay/           # 介质 SSH 中转(原子可见推送)
│   ├── webhook/              # Webhook 签名投递(HMAC-SHA256、退避)
│   ├── cli/                  # CLI(纯 HTTP 客户端,契约之外零语义)
│   ├── config/               # 环境变量配置(MAMMOTH_*)
│   └── obs/                  # slog/Prometheus/OTel 可观测埋点
├── docs/                     # 设计文档(本目录)+ compat/ 厂商与发行版矩阵
└── deploy/                   # 容器与 compose 示例
```

## 5. 与其他文档的一致性

- [02-architecture.md](02-architecture.md):依赖组件表已随 D2/D3 修订
  (Redis 移除,队列与协调由 PostgreSQL 承担);
- [09-roadmap.md](09-roadmap.md):M6 的"最小部署模式" = SQLite 后端 + 内嵌表队列;
- [03-api.md](03-api.md):契约生成工具链即 oapi-codegen,spec 文件位于 `api/openapi.yaml`。
