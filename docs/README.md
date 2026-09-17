# Mammoth 设计文档

> **Mammoth** — A self-contained bare-metal provisioning engine.
> Send in an address and a credential, get back a machine that runs.

Mammoth 是一个自包含的裸金属服务器安装/重装引擎:以 `{带外地址, 凭证}` 的最小输入接管机器,
自动盘查硬件与磁盘布局,按声明式意图(分区/网络/身份)完成操作系统安装与重装,
并对外封装开关机、重启、虚拟介质等通用带外操作。

**API-first,无内置 UI**。前端界面由集成方基于 OpenAPI 契约构建;本项目交付 API、SDK 与 CLI。

## 文档导航

| 文档 | 内容 |
|------|------|
| [01-overview.md](01-overview.md) | 项目定位、能力总览、设计原则、明确不做什么 |
| [02-architecture.md](02-architecture.md) | 总体架构:控制面/执行面/构建面分离、任务状态机、部署形态 |
| [03-api.md](03-api.md) | API 设计规范:资源模型、URL 空间、动作语义、横切规范 |
| [04-install-spec.md](04-install-spec.md) | 数据契约:Machine/Credential/Layout 与 Install Spec(storage/network/…) |
| [05-inventory.md](05-inventory.md) | 盘查体系:三探针(Redfish / inband-ssh / ramdisk)与分区快照 |
| [06-install-pipeline.md](06-install-pipeline.md) | 安装流水线:介质策略、应答文件渲染、分区校验、发行版驱动 |
| [07-bmc.md](07-bmc.md) | BMC 驱动层:统一带外资源模型、Redfish/IPMI 适配、兼容矩阵 |
| [08-data-model.md](08-data-model.md) | 内部数据模型:表结构、索引、保留策略 |
| [09-roadmap.md](09-roadmap.md) | 演进路线:里程碑划分、当前状态与各阶段验收标准 |
| [10-tech-stack.md](10-tech-stack.md) | 技术栈选型、决策记录、依赖树与仓库布局约定 |
| [11-pxe-walkthrough.md](11-pxe-walkthrough.md) | PXE 导览:两条入门路径对比、完整引导接力链、零注册入门、跨网段/Relay |
| [operations.md](operations.md) | 运维手册:备份恢复、介质服务形态、部署注意 |
| [security-baseline.md](security-baseline.md) | 安全基线:鉴权、凭证加密、介质生命周期、审查余项 |
| [compat/patterns.md](compat/patterns.md) | 跨厂商装机共性模式(P1 控制器卷名/P2 DHCP 竞态/P3 参数截断…)——新机型/新方言自查清单 |
| [runbooks/test-baselines.md](runbooks/test-baselines.md) | 整机回归基线:主流服务器镜像矩阵/镜像类型×载体法则/寻址核对单/通过标准 |
| [related-work.md](related-work.md) | 与 Ironic/Metal3/Tinkerbell/Pixiecore 的对照与借鉴 |
| [compat/](compat/README.md) | 厂商兼容矩阵(huawei 实录)与发行版支持矩阵(distros) |
| [runbooks/](runbooks/) | 真机回归操作清单(按方言;ubuntu22 首篇) |

## 设计参考

Mammoth 站在巨人的肩膀上,关键机制均有业界先例:

- **OpenStack Ironic / Metal3 BareMetalHost** — 裸机域模型与 provision state 机
- **Tinkerbell** — 通用 ramdisk + 元数据注入,介质与机器解耦
- **cloud-init network-config v2** — 网络声明式 schema 的事实标准
- **Google AIP** — 资源命名、长任务(operation)与 API 语义
- **Stripe API / RFC 9457** — 凭证管理、幂等、错误结构
