# scripts/windows-dev — Windows 通路开发与 E2E 工具集

Windows Server 装机通路(docs/compat/distros.md §windows)的开发/验证工具:
qemu 侧的启动验证环、外部逃生门 E2E 环,以及真机提交体样例。
构建链侧的 Go 兜底测试在 `internal/builder/windows_dev_test.go`(两者互补)。

## 组成

| 文件 | 作用 |
|------|------|
| `qemu-win.sh` | BOOT 侧验证环:OVMF → bootmgfw → WinPE → setup 全链 qemu 引导,带 monitor 控制、screendump 看门狗与完成回调接收器;无 /dev/kvm 时自动退 TCG。环境变量:`WIN_DIR`(工作目录,默认 /data/mammoth/win-dev)、`WIN_ISO`(引导 ISO) |
| `external-win-e2e.sh` | 外部逃生门 E2E(特权 alpine 容器):站点 DHCP+TFTP(dnsmasq)→ plain iPXE → wimboot 形态 per-MAC 脚本 → WinPE,全链路走通;含 capabilities 中提到的 e1000 网卡/4G 内存等 rig 层约束 |
| `2288h-install.json` | 真机安装任务的提交体样例(PXE + wimboot 载体,ESP+NTFS 布局,地址为文档示例段) |

## 依赖

qemu-system-x86_64(或 CentOS 的 qemu-kvm)、wimlib-imagex;external E2E 另需
alpine 容器内的 dnsmasq/socat/samba。Secure Boot 需关闭(wimboot 无签名)。

## 相关文档

- 通路与载体决策:`docs/compat/distros.md` §windows
- 提交体字段:`docs/04-install-spec.md`
- 真机排障:`docs/runbooks/` 下对应机型 runbook
