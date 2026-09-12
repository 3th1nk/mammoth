# 02 · 总体架构

## 1. 三面分离

Mammoth 的运行时由三个职责面组成,可同进程运行,也可独立部署与扩缩容:

```
                ┌─────────────────────────────────────────┐
   API 客户端 ──▶│ 控制面 api(无状态,水平扩展)              │
                │  资源 CRUD、校验、意图受理、状态聚合        │
                │  事件流(SSE)/ Webhook 投递               │
                └───────┬─────────────────────────────────┘
                        │ 持久化队列(job/task 指令)
        ┌───────────────┼───────────────────────────┐
        ▼               ▼                           ▼
┌──────────────┐ ┌───────────────┐        ┌────────────────────┐
│ 执行面 runner │ │ 构建面 builder │        │ 盘查面 prober(可选) │
│ 状态机推进    │ │ 应答文件渲染    │        │ inband-ssh 探针     │
│ BMC 操作      │ │ 引导介质组装    │        │ 冗余快照刷新         │
│ 安装等待与确认 │ │ 介质分发       │        │                    │
└──────┬───────┘ └───────────────┘        └────────────────────┘
       │ stage 推进 / 心跳
       ▼
 PostgreSQL(状态/契约/队列)      介质仓库(本地卷/对象存储)
```

**职责边界**

- **控制面**不触碰 BMC、不触碰文件系统:只做校验、受理、聚合。它随时可以多副本。
- **runner** 从队列领取 task,推进状态机。每个 task 有心跳;控制面的 reaper 将心跳超时的
  task 标记为 `interrupted`,使其可被重试——进程崩溃不产生永久悬挂任务。
- **builder** 独立消费"介质准备"子任务。渲染与介质组装是 CPU/IO 密集操作,独立成面后
  可单独扩容,且把外部工具依赖(xorriso 等)圈定在专用部署单元内。
- **prober** 承担带内盘查(SSH 探针)。逻辑简单但涉及外部网络访问,独立部署便于网络分区与限流。

**为什么控制面与执行面必须分离**

裸机安装的单任务时长以十分钟计,且强依赖外部网络与 BMC 的响应速度。若在 API 进程内执行,
将同时遭遇:水平扩展时的任务重复执行、进程重启导致的任务悬挂、以及 API 延迟被长任务拖垮。
队列化 + 状态机 + 心跳是这三者的统一解。

## 2. 任务模型

```
job(type: install | power | discover)
 └── task(per machine)
      └── stages(有序阶段序列,持久化推进)
           verify_layout → configure_raid → prepare_media → boot → install_os → verify_ready
```

核心规则:

1. **stage 序列由 flow 定义声明**,枚举与执行序列收口在同一处,不存在两套阶段表;
2. task 持久化 `stage_index / stage_attempt / stage_deadline / heartbeat_at / owner_runner`;
3. **幂等执行**:每个 stage 的 `Do()` 必须可在重试时安全重入;
4. **补偿执行**:每个 stage 可声明 `Compensate()`(弹出虚拟介质、清除引导设置),取消任务时逐级回滚;
5. 重试 = 从 `stage_index` 重新入队;取消 = 停止调度 + 补偿;
6. 乐观锁推进:`UPDATE ... WHERE stage_index = ?`,并发推进不可能跳阶段。

## 3. 安装数据流

```
InstallSpec(声明式意图)
   │ 控制面校验(schema + 语义约束 + layout 快照引用;install-plan 可试算)
   ▼
渲染(build 面)            ┌─ storage ──▶ 分区动作集(执行时 %pre 解析校验)
   spec × machine layout ──┤  network ──▶ 应答文件 network 段(netplan/network 命令/netcfg)
                          └─ identity ─▶ 主机名/凭证/脚本
   ▼
发行版原盘重打包为 boot-<token>.iso(应答文件烘入,离线 seed)
   ▼
BMC 虚拟介质挂载 + 一次性引导(介质服务:内置 NFS 导出 / 外部 NFS / SSH 中转)
   ▼
安装环境 %pre:校验实际分区表 vs 快照 ─── 不一致 ──▶ 中止回报(LAYOUT_DRIFT)
   ▼                       │ 一致
   动态生成分区指令 ◀───────┘
   ▼
安装执行 ──▶ 完成回调 ──▶ verify_ready(带内探活轮询 + 装后快照刷新)──▶ task.succeeded
```

要点:**最终分区指令不在提交时烧死**。提交时绑定的是"意图 + 快照基线",
执行时以 `%pre` 钩子对实际布局做强校验后动态生成——把"信息过期"从静默错误变成显式失败。

## 4. 依赖组件

| 组件 | 用途 | 说明 |
|------|------|------|
| PostgreSQL ≥14 | 主存储 + 任务队列 + 分布式协调 | **唯一运行时强依赖**:资源/契约/任务状态/事件存于同一库;队列用 `SKIP LOCKED` 表队列,互斥用 advisory lock;repo 接口抽象(仅 PostgreSQL,SQLite 最小部署已评估并放弃,见 10 §D2) |
| 介质仓库 | 发行版原盘、任务引导介质(`boot-<token>.iso`) | 本地卷(`MAMMOTH_MEDIA_DIR`);S3 兼容对象存储为规划后端 |
| 介质服务 | 让 BMC 可挂载介质 | 内置只读 NFSv3 导出(`internal/nfsx`,go-nfs + mini rpcbind,默认开)/ 外部 NFS(`MAMMOTH_MEDIA_BASE_URI`)/ SSH 中转(`MAMMOTH_MEDIA_RELAY_*`,原子可见) |
| 凭证加密 | credential 的静态加密 | 主密钥由部署方配置(`MAMMOTH_MASTER_KEY`;**轮换后存量凭证须重建**) |

> 队列与协调由数据库承担的完整理由见 [10-tech-stack.md](10-tech-stack.md) D2/D3:
> 部署单元从"存储 + 队列"两个服务降为一个,且任务状态与队列消息同库同事务,
> 机制上消除双写不一致。

## 5. 部署形态

### 5.1 单二进制,多运行模式

```
mammoth serve --mode=all        # 单机一体化(评估/小规模)
mammoth serve --mode=api        # 控制面
mammoth serve --mode=runner     # 执行面
mammoth serve --mode=builder    # 构建面(需要 xorriso,用户态装配无特权要求)
mammoth serve --mode=prober     # 盘查面
```

- 容器化交付;builder 容器仅需 xorriso(纯用户态探测/提取/装配,不 loop mount),见 [06-install-pipeline.md](06-install-pipeline.md);
- runner/builder/prober 与 api 同二进制、不同入口,共享一份配置与契约,避免分叉。

### 5.2 可观测性

| 维度 | 实现 |
|------|------|
| 日志 | 结构化(JSON);标准字段集(`task_id` `machine_id` `job_id` `request_id` `stage`)全链路透传;任务日志双写:存储(供 API 检索)+ 本地日志 |
| 指标 | Prometheus:job/task 计数(按 state)、各 stage 耗时直方图、`bmc_request_duration_seconds`(按 vendor/操作/结果)、`bmc_errors_total`(按错误码)、队列深度 |
| 追踪 | OTel API 边界埋点(HTTP handler / 队列消费 / BMC 调用 / 渲染),默认 no-op 零依赖;配置启用 OTLP 导出后生效;span 上下文随队列消息透传,跨面不断链 |
| 事件 | stage 变更与终态写入事件表,经 SSE 推送(job 级 + 全局,Last-Event-ID 续传);Webhook 订阅投递(HMAC-SHA256 签名、类型过滤、退避重试) |
| 审计 | 全部写操作(凭证创建、动作下发、job 提交)记审计事件:who/when/what/target |

> 埋点自 M0 写入代码(见 [10-tech-stack.md](10-tech-stack.md) D6):
> 长链路异步系统的链路关联必须第一天建立,事后补埋点的成本远高于埋点本身。

### 5.3 容器交付

单镜像多模式为主,builder 独立镜像为辅:

| 镜像 | 内容 | 基础镜像 | 说明 |
|------|------|---------|------|
| `mammoth` | 主二进制,`--mode=all/api/runner/prober` 共用 | distroless(静态编译,无 shell) | 业务面无外部命令依赖,可最小化 |
| `mammoth-builder` | 主二进制 `--mode=builder` + xorriso | alpine/debian-slim | 需要外部工具 xorriso(用户态装配,无特权要求) |

- 多架构:amd64 + arm64(goreleaser → 多架构 manifest);
- 配置全部经环境变量与挂载配置文件注入,镜像无状态;
- docker compose 示例(`deploy/`)覆盖:一体化(all-in-one + PG)、分面(1×api + N×runner + 1×builder)两种拓扑;
- 健康检查:`/healthz`(存活,无依赖检查)与 `/readyz`(就绪,DB/队列可达)。

### 5.4 安全基线

- API 鉴权:Bearer token;token 管理为部署方责任(反向代理或内置静态 token,首版内置);
- 凭证静态加密,API 永不回显 secret;
- 引导介质中的临时凭证一次性、任务结束即失效;
- 带内盘查使用最小权限只读命令集,不向目标机写入任何文件。
