<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
    <img src="assets/brand/logo.svg" alt="mammoth logo" width="140">
  </picture>
</p>

# Mammoth

> 自包含的裸金属服务器安装引擎。
> 输入一个地址和一份凭证,还你一台能跑起来的机器。

[**English**](README.md)

Mammoth 通过带外控制器(BMC)接管机器,自动盘查硬件与磁盘布局,并按声明式意图
安装/重装操作系统——同时把开关机、引导设备、虚拟介质等通用带外能力做成
一等公民 API。**纯后端,API-first;无内置 UI。**

- 设计文档:[docs/README.md](docs/README.md)
- API 契约(唯一事实源):[api/openapi.yaml](api/openapi.yaml)
- 状态:**v1.0 就绪** —— M0~M6 全部交付(契约冻结),三方言(rocky9 / ubuntu22 / debian12)
  真机端到端闭环;M7 PXE/iPXE 网络引导已交付并真机闭环。([路线图](docs/09-roadmap.md))

## 快速开始(一体化)

前置条件:Go ≥ 1.26,Docker(用于 PostgreSQL)。

```bash
docker run -d --name mammoth-pg -e POSTGRES_USER=mammoth -e POSTGRES_PASSWORD=mammoth \
  -e POSTGRES_DB=mammoth -p 5432:5432 postgres:16-alpine

go build -o bin/mammoth ./cmd/mammoth

MAMMOTH_DATABASE_URL='postgres://mammoth:mammoth@localhost:5432/mammoth?sslmode=disable' \
MAMMOTH_API_TOKEN=devtoken \
MAMMOTH_MASTER_KEY=$(openssl rand -base64 32) \
bin/mammoth serve --mode=all
```

配置采用 12-factor 环境变量(`MAMMOTH_*`)。裸二进制部署也可用 dotenv 文件——
已存在的环境变量优先于文件条目,非法值会在启动时报错而非静默默认:

```bash
bin/mammoth serve --env-file /etc/mammoth.env   # 或 MAMMOTH_ENV_FILE=...
```

试一下:

```bash
TOKEN='Authorization: Bearer devtoken'
# 1. 注册一份凭证(只写;永不回显)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"bmc","name":"demo","secret":{"username":"admin","password":"..."}}' \
  localhost:8080/api/v1/credentials
# 2. 注册一台机器(protocol=fake → 内置 BMC 模拟器)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"bmc":{"address":"fake://n1","protocol":"fake","credential_id":"cred_..."}}' \
  localhost:8080/api/v1/machines
# 3. 开机(异步:202 + job)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"power_on"}' localhost:8080/api/v1/machines/mch_.../actions
# 4. 观察 job
curl -s -H "$TOKEN" localhost:8080/api/v1/jobs/job_...
```

运行脚本化验收流程(含 SIGKILL → reaper → retry 证明):

```bash
MAMMOTH_FAKE_BMC_DELAY=8s bin/mammoth serve --mode=all &   # 慢速 fake BMC
python3 scripts/acceptance.py --api http://localhost:8080 --token devtoken \
  --dsn 'postgres://mammoth:mammoth@localhost:5432/mammoth?sslmode=disable'
```

## 一屏看懂架构

```
client ──▶ api (控制面, 无状态)                    ──┐
            runner (经 BMC 执行任务)               ──┼──▶ PostgreSQL (状态 + 表队列)
            builder (介质构建)                     ──┤
            prober (带内盘查)                      ──┘
```

- 单二进制多模式:`serve --mode=all|api|runner|builder|prober`
- 至少一次投递的任务队列 + 可见性超时(PG `SKIP LOCKED`);状态流转与队列操作共享同一事务域
- 每个任务都有心跳;reaper 将失联任务标记为 `interrupted`(可重试)——runner 崩溃也不会让任务永久挂起
- Redfish 优先,IPMI 兜底;`fake` 驱动用于开发与 CI
- 每次安装两条引导载体:BMC 虚拟介质(默认)或 PXE/iPXE 网络引导(内置 proxyDHCP + TFTP,Pixiecore 风格;经 `MAMMOTH_PXE_ENABLED` 开启——见 docs/operations.md §4.5)

## 交付物

| 镜像 | 基础镜像 | 模式 | 说明 |
|------|---------|------|------|
| `mammoth` | distroless(静态) | all / api / runner / prober | 无外部工具 |
| `mammoth-builder` | alpine + xorriso | builder | 特权,用于介质构建 |

```bash
docker compose -f deploy/compose.all-in-one.yml up -d   # 1×all + PG
docker compose -f deploy/compose.faceted.yml up -d      # 1×api + N×runner + 1×builder
```

受限网络:`--build-arg RUNTIME_IMAGE=<mirror>/distroless/static-debian12:nonroot`
与 `--build-arg GOPROXY=https://goproxy.cn,direct`。

## 开发

```bash
make build        # bin/mammoth
make test         # 单元测试(无外部服务)
make test-pg      # queue/store 契约测试套件,针对一次性 PG
make generate     # 从 api/openapi.yaml 重新生成(契约漂移会导致 CI 失败)
make acceptance   # 完整脚本化验收流程,针对本地 all-in-one
```

可观测性:JSON 日志带标准字段(`task_id` `machine_id` `job_id` `request_id` `stage`),
Prometheus 于 `/metrics`,OTel 边界埋点(默认 no-op,`MAMMOTH_OTEL_EXPORTER_ENDPOINT` 导出)。

## 许可证

Apache-2.0

### Logo

mammoth gopher 是 **Renée French** 原作 Go gopher(CC BY 3.0)的衍生作品,
以相同许可证改编。见 `assets/brand/`。
