# 安全策略

## 支持的版本

| 版本 | 支持 |
|------|------|
| v1.x | ✅ |

## 报告漏洞

**请勿通过公开 issue 报告安全漏洞。**

请使用 GitHub 的私有漏洞报告(仓库 Security 标签页 → Report a vulnerability),
或通过仓库维护者的 GitHub 账号私下联系。报告时请附:

- 影响的版本(commit 或 tag)
- 复现步骤或概念验证
- 影响面评估(哪些部署形态受影响)

我们会在收到报告后确认并反馈处理进度;修复发布后在 Release 说明中致谢(除非你希望匿名)。

## 安全基线

部署侧的安全基线(TLS 终止、API token、主密钥与凭证加密、PXE/DHCP 暴露面、
最小权限等)见 [docs/security-baseline.md](docs/security-baseline.md)。
该文档同时是本仓库自身的安全审查记录。

## 设计上的安全边界

- 凭证只写不可读:凭证以 `MAMMOTH_MASTER_KEY` 派生密钥加密落库,API 永不回读明文;
  主密钥轮换后存量凭证不可解(需重建凭证,见 docs/operations.md)。
- 机器面端点(`/render/{token}/...`)以不可猜测的 task token 为凭证,
  与控制面 Bearer token 相互独立。
- 高危 BMC 动作(BIOS 写入、盘擦除)默认两段式确认:请求缺省即 422,不建 job;
  执行期还有活表二次校验,策略配置关不掉。
- 完成回调与装后核验永远指向机器的现实地址(RemoteIP 而非可伪造的转发头)。
