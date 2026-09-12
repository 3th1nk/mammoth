# 安全基线(v1.0 门槛审查记录)

> docs/09-roadmap.md 的 v1.0 门槛要求"安全基线审查完成"。本文是该审查的
> 结论快照:每项的现状、依据与已知余项。发布 v1.0 前需复审一次。

## 1. 鉴权与访问控制

| 项 | 现状 | 依据 |
|----|------|------|
| API 鉴权 | 静态 Bearer token(`MAMMOTH_API_TOKEN`);未配置时受保护路由返回 503 而非放行 | docs/02-architecture.md §5.4 |
| 免鉴权面 | 仅 `/healthz` `/readyz`(编排器探活)与 `/render/{token}/...`(机器面) | internal/api.BearerAuth |
| 机器面凭证 | 应答文件/完成回调以路径中的 task token 为凭证:128 位十六进制随机数、任务级隔离、随任务上下文持久化(可审计) | docs/06-install-pipeline.md §2.1 |
| 外部 IdP | 预留适配点(中间件单点替换);v1.0 未实现 | docs/03-api.md §4 |
| 多租户 | 无;单部署单信任域,RAID 级隔离属于部署方 | — |

**余项**:token 生命周期管理(轮换/吊销)目前依赖部署方(重启进程换 token);
rate limiting 依赖反向代理。

## 2. 凭证与机密

| 项 | 现状 |
|----|------|
| 静态加密 | AES-256-GCM,主密钥部署方注入(`MAMMOTH_MASTER_KEY`,base64 32B) |
| 密钥不回显 | credential API 永不返回 secret;明文仅在 BMC 调用边界解密 |
| 传输 | 生产要求 HTTPS 在反向代理终止(mammoth 自身监听 HTTP;distroless 内无证书管理) |
| 一次性口令 | `root_password` 留空 = 任务级随机,经任务事件一次性下发(字段为纯明文字符串,无 generate 哨兵值),应答文件(ks.cfg / user-data / preseed.cfg)是唯一持久副本(任务结束后可随事件 TTL 过期;应答文件烘入引导介质,介质随 verify_ready 删除) |
| Host key | 带内 SSH 首连即接受(重装生命周期密钥轮换);严格指纹为部署层策略 |
| 引导介质凭证 | 内核参数只携带应答文件位置(token 介质上的路径),不携带 BMC/系统凭证;口令随应答文件在介质内(离线 seed 的取舍,介质生命周期=任务生命周期) |

## 3. 输入与注入面

| 面 | 缓解 |
|----|------|
| 应答文件注入(kickstart/autoinstall/preseed) | 模板变量来自结构化 spec;文本注入是**明确接受的风险**——spec 提交者即基础设施管理者(见 01-overview 明确不做的:不做多租户)。`%pre`/curtin/preseed 钩子脚本由服务端生成,不拼接用户 shell |
| shell 注入(带内探针) | 命令集是编译期常量,禁止参数拼接(有测试守护) |
| SQL 注入 | 全部查询参数化($n/?),无字符串拼接值 |
| 渲染注入(模板) | text/template 自动转义不适用于 shell 场景;注入面同上(提交者=管理者) |

## 4. 运行时边界

- 控制面不触碰 BMC、不触碰文件系统(docs/02 §1 职责边界);
- 单台 BMC 异常被隔离:同址串行 + 每调用超时 + 错误分类,不拖垮 runner;
- distroless 主镜像:无 shell、无包管理器,builder 镜像(含 xorriso)单独特权域;
- 队列与状态同库同事务:无双写不一致面。

## 5. 发布前复审清单(v1.0)

- [x] Bearer 鉴权覆盖所有非探活/机器面路由
- [x] 凭证静态加密 + 永不回显
- [x] task token 熵与任务级隔离
- [x] SQL/命令注入面清点
- [x] 备份恢复文档(含主密钥关键性)
- [ ] HTTPS 终止样例( compose + 反向代理示例)
- [ ] 依赖漏洞扫描进 CI(govulncheck)
- [ ] token 轮换工具
