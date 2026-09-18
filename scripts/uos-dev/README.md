# uos-dev · UOS 定制 anaconda Finish 崩溃复现(qemu,不占真机窗口)

uniontechos blocked 的根因在 UOS 定制 anaconda 内部(Finish 任务组,
`dasbus.error.DBusError: max() arg is an empty sequence`,task_proxy.Finish
——实录见 docs/compat/distros.md §uniontechos),与投递载体无关。本 harness
把复现搬进 qemu:**锚定 1050a 崩溃拿到 anaconda-tb 深层帧 → 筛 1050u1a/u2a
新版是否已修**,全程不消耗 2288H 窗口。

## 用法

```bash
# 1. 安装阶段(图形 anaconda,-display none;ks 走 slirp http,日志走串口+syslog)
UOS_ISO=~/mammoth-qxe/uos/uniontechos-server-20-1050a-amd64.iso \
  scripts/uos-dev/uos-qemu.sh install

tail -f ~/mammoth-qxe/uos-run/logs/serial.log      # 安装进度(anaconda 主进程日志)
scripts/uos-dev/uos-qemu.sh shot                    # screendump → uos-run/screen.png

# 2. 判定
#   - qemu 退出(rc=0,-no-reboot 下 anaconda 请求重启)= 安装走完 → 进入 boot 阶段
#   - qemu 挂着不动 + serial.log 出现 Traceback/dasbus + screendump 是 crash 对话框
#     = 复现(预期 1050a 如此)
# 3. 崩溃现场取证(第二终端)
ssh -p 2222 root@localhost   # 密码见 uos.ks(inst.sshd;安装器 env)
ls /tmp/anaconda-tb-*        # 深层 traceback——给 UOS 报告的入场券
# 4. 安装完成(或换新版 ISO 筛过后)验证可引导
scripts/uos-dev/uos-qemu.sh boot && ssh -p 2222 root@localhost
```

## 设计要点

- **`-kernel` 直启**:从 ISO 提取 `images/pxeboot/{vmlinuz,initrd.img}`
  (与 stage2 同源),anaconda 参数走 `-append`,不碰引导菜单;
  `inst.stage2=hd:LABEL=<PVD 卷标>` 定位 ISO(卷标从 ISO 9660 PVD
  偏移 32808 读出)。
- **图形保真**:崩溃栈在 task_proxy(UI↔安装任务经 DBus),不赌 text
  模式,图形安装器跑在 VGA 上(`-display none` 不影响 screendump);
  `shot` 子命令经 monitor socket `screendump` + sips 转 PNG。
  ⚠️ **UOS 只能用图形前端**:text 模式(text 指令/inst.text)下 Finish
  任务组恒崩(`max()` 空序列)且 autopart 建出 FAT16 而非 swap——
  mammoth 驱动已强制 graphical;人工手写 ks 排查时同样必须遵守。
- **观测三通道**:串口日志(主进程消息)、`inst.syslog=10.0.2.2:5141`
  (rsyslog 转发,host nc 收)、`inst.sshd` + hostfwd 2222(崩溃后
  `/tmp/anaconda-tb-*` 取证)。
- **pass 信号 = -no-reboot 下的 qemu 退出**(anaconda 完成 → 请求重启);
  crash = qemu 悬挂 + crash 对话框截屏。
- Apple Silicon 上是 TCG(无 KVM),489 包量级安装需 1~3 小时,后台挂夜。

## 事件线(2026-09-18)

- ISO 源:248 `/data/os_iso/统信服务器操作系统V20-1050a/` 四版齐备
  (1020a-HCI / **1050a**(真机崩过的版本)/ 1050u1a / **1050u2a**)。
  本机到 248 单流 scp 仅 ~730KB/s,采用 4 流 dd 分块并行(≈4MB/s)。
- 计划:1050a 锚定复现(拿 traceback)→ 1050u2a 筛版(若过,真机
  窗口的 uniontechos 项升级为"u2a 复验 + PXE 顺带复核")。
