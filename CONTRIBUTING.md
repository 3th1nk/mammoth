# 贡献指南

感谢关注 Mammoth。本文描述开发环境的搭建方式与本仓库的工程约定。

## 开发环境

- Go ≥ 1.26
- PostgreSQL ≥ 14(存储为 PostgreSQL-only;SQLite 最小部署形态已评估并放弃)
- Docker(可选,用于 PG 集成测试与容器拓扑)
- xorriso / 7z(仅 builder 介质重打包路径需要;单测不依赖)

## 常用命令

| 命令 | 作用 |
|------|------|
| `make build` | 编译 `cmd/mammoth`(serve 与 CLI 同二进制) |
| `make generate` | 由 `api/openapi.yaml` 再生服务端/类型层(oapi-codegen) |
| `make contract-check` | 契约漂移检查——`api/openapi.yaml` 是唯一事实源,手写层漂移即红 |
| `make test` | 单元测试 |
| `make test-pg` | 需要真实 PostgreSQL 的契约测试套件 |
| `make fmt-check` / `make vet` / `make lint` | 静态检查 |
| `make vuln-check` | govulncheck 漏洞扫描 |
| `make acceptance` | 验收冒烟(构建后执行) |

提交前请跑:`make lint && make test`;涉及契约或存储层的改动另跑 `make test-pg`。

## 工程约定

- **文档用中文,代码、标识符、API 字段用英文。**
- **契约先行**:`api/openapi.yaml` 是 API 的唯一事实源;生成层与手写层物理隔离,
  不要手改 `internal/api/gen/` 下的任何文件。
- 设计文档放 `docs/`(编号序列,见 [docs/README.md](docs/README.md));
  厂商与发行版兼容矩阵放 `docs/compat/`;真机回归清单放 `docs/runbooks/`。
- **设计动机一律用通用工程语言表达**。引用公开技术(kickstart / preseed /
  autoinstall / unattend / Redfish / IPMI)属正常引用;Ironic / Tinkerbell /
  Metal3 / cloud-init 等先例项目在设计文档中作为先例引用是加分项。
- 存储层改动必须带迁移文件(单调递增编号)并遵守三铁律
  (快照不可变 / 心跳所有权 / 单写者推进,见 docs/08)。
- 新增发行版接入见 docs/06(驱动注册制);行为开关与实现同址,
  不引入与代码分离的行为声明文件。
- 可观测: slog 标准字段集 + Prometheus 指标;携带 `task_id` 的日志行
  会经 `obs.TaskLogTee` 落 task_logs。

## 提交与评审

1. Fork 后建分支;提交信息用 `类型(范围): 摘要` 形式(如 `docs(api): ...`、`fix(netboot): ...`)。
2. 一个提交聚焦一件事;行为改动必须带测试。
3. PR 描述说明动机与验证方式(qemu harness / 单测 / 真机轮次)。
4. CI 会跑契约漂移检查、静态检查、单测、PG 集成与 govulncheck,全绿后方可合并。

## 安全问题

不要用公开 issue 报告安全漏洞,见 [SECURITY.md](SECURITY.md)。
