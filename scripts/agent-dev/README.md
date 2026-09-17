# agent-dev · agent initramfs 试点 qemu 验证 harness(docs/12-agent-initramfs.md §4)

一条命令跑通"提交 → 渲染 plan → 重打包 ISO → qemu 引导 → agent 装机 →
回调 → 六阶段 → 重启 SSH 验证",BIOS/UEFI 双固件:

```sh
AGENT_E2E_FIRMWARES="bios uefi" ./scripts/agent-dev/run.sh
```

前置:

- alpine **extended** ISO(standard 的 /apks 是 boot 池,装不了系统):
  `~/mammoth-qxe/alpine-extended-3.22.2-x86_64.iso`(可用
  `AGENT_E2E_ISO` 覆盖)
- 独立数据库(防真机侧 runner 抢队列,见 docs/compat/patterns.md P 系
  环境教训):`createdb -h localhost -p 55432 -U mammoth agent_e2e`
- qemu-system-x86_64 + xorriso + ssh-keygen(Homebrew qemu 的 UEFI vars
  在 `edk2-i386-vars.fd`,harness 已自适应)

环境变量:`AGENT_E2E_WORK`(默认 /tmp/agent-e2e)、`AGENT_E2E_API`、
`AGENT_E2E_DSN`、`AGENT_E2E_ISO`、`AGENT_E2E_FIRMWARES`。

产物:每固件一目录(`$WORK/<fw>/`)——serial 全程日志、盘镜像、
agent-plan.sh 抽取件;服务端日志在 `$WORK/logs/server.log`。

注意:machine 地址带时间戳避免 409;qemu 无 KVM(macOS arm64 宿主),
TCG 全程约 3-4 分钟/固件。
