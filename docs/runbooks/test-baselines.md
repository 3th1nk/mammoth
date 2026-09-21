# 整机测试基线(主流服务器镜像)

> 目的:规定每轮真机回归**用哪些镜像、覆盖哪些载体、通过标准是什么**,
> 使不同时间、不同操作者的测试结果可比。README 的"回归基线"表是本文件的
> 摘要;细节与判定标准在此。

## 0. spec 口径注记(真机轮实证,2026-09-19)

- **ubuntu(autoinstall)的 /boot/efi 必须是 ESP**——curtin 按 flag 定型,
  不像 anaconda 从挂载点推断;漏写会得到 "did not create needed bootloader
  partition"。渲染层已自动补(见 normalizeESP),手写 spec 时也建议显式;
- **ubuntu PXE 装机的 spec 必须带 network 声明**——无声明时安装环境经
  池 DHCP 取址,subiquity 继承进 target 的地址随池租约漂移(真机实证
  spec .211 → 实装 .212),与"spec 静态 > 池 > DHCP"的三层寻址相悖;
- agent(alpine)spec 同上必须带 network——plan 无网络则装后回调不可达
  (回调 curl 带 10s 超时,自恢复重启,但完成状态丢失)。

## 1. 基线矩阵

| # | 家族 | 发行版 | 镜像(最新点版本) | 镜像类型 | 载体覆盖 | 真机状态 |
|---|------|--------|--------------------|----------|----------|----------|
| 1 | RHEL 系 | rocky 9.x | Rocky-9.x-x86_64-minimal.iso | minimal(自带包池) | vMedia ✅ / PXE ✅ | 2026-09-10 起 |
| 2 | Ubuntu 系 | ubuntu 22.04.x | ubuntu-22.04.x-live-server-amd64.iso | live-server(casper) | vMedia ✅ / PXE ✅ | 2026-09-12 / 09-17 |
| 3 | Ubuntu 系 | ubuntu 24.04.x | ubuntu-24.04.x-live-server-amd64.iso | live-server(casper) | vMedia ✅ / PXE ✅ | 2026-09-17 |
| 4 | Debian 系 | debian 12 / 13 | debian-1x.x-amd64-netinst.iso | netinst(自带包池) | vMedia ✅ / PXE ✅(13) | 2026-09-12 / 09-16 |
| 5 | 扩展 | rocky 10 | Rocky-10.x-dvd.iso | DVD,UEFI-only | vMedia ✅ / PXE 待验 | qemu ✅ |
| 6 | 扩展 | centos 7 | CentOS-7-x86_64-Minimal.iso | minimal | vMedia ✅ | legacy |
| 7 | 扩展 | kylin V10/V11 · UOS | 厂商 DVD | DVD | vMedia | 按客户需求 |
| 8 | Windows 系 | windows server 2019 | cn_windows_server_2019_x64_dvd.iso | Windows Setup(install.wim) | PXE(wimboot+SMB 源)✅ / vMedia 🔧(iBMC 6.41 固件缺陷) | 2026-09-21 |

## 2. 镜像类型 × 载体选择法则

| 镜像类型 | 内含物 | virtual_media | PXE |
|----------|--------|---------------|-----|
| netinst / minimal | 安装器 + 自带小包池 | ✅ 重打包 | ✅ 引导文件抽取 + 池即安装源 |
| DVD | 完整离线池 | ✅ 重打包 | ✅(离线包池) |
| live-server(ubuntu) | casper 安装器载体 | ✅ 重打包 | ✅ squashfs 走 NFS |
| live desktop | 无安装器 | ❌ | ❌ |
| Windows(install.wim) | Windows Setup 安装器 + 镜像库 | ✅ 重打包(El Torito) | ✅ wimboot 引导 + SMB 安装源 |

- windows PXE 额外依赖:部署层只读 SMB 导出介质仓库(`MAMMOTH_WINDOWS_INSTALL_SMB_UNC`
  三元,可选专用账号);WinPE 内存 ≥4G;完成回调经 AutoLogon+FirstLogonCommands
  或 SetupComplete(双路径冗余,均执行 mammoth-complete.ps1)。

- debian PXE 额外依赖:官方 `netboot.tar.gz`(`MAMMOTH_PXE_DI_NETBOOT`)
  与 udeb 暂存目录(`MAMMOTH_PXE_DI_UDEBS_DIR`,fetch-di-udebs.sh)。
- ubuntu PXE 额外要求:介质基址可 NFS(`MAMMOTH_MEDIA_BASE_URI`)。
- 禁用 live desktop 类镜像:其内无安装器,两条载体都会空转。

## 3. 寻址模式核对单(每轮开机前)

1. 装机网段是否已有 site DHCP?
   - 无 → `MAMMOTH_PXE_DHCP_POOL=<空闲段>`(池模式);段内静态设备先用
     `ping + ip neigh` 双查排除。
   - 有 → 不配池(proxy 模式);装机地址在 spec.network 声明静态
     (netplan 与引导参数同步生效)。
2. 池段/声明地址不得与 site DHCP 池、已知静态设备重叠。
3. 跨 VLAN:机器网段配 `ip helper` 指向 mammoth(:67 与 4011);
   NextServer/ExternalURL 从机器网段可达;应答走 giaddr 回程(已实现)。

## 4. 通过标准(缺一即回归失败)

1. 六阶段全绿:verify_layout → configure_raid → prepare_media → boot →
   install_os → verify_ready;
2. 完成回调到达且 status=ok;
3. 重启后**无人值守**进入新系统(不得停在 grub/交互提示符);
4. 装机凭据 SSH 可登录(spec 钥匙或密码),主机名与 spec 一致;
5. 装后布局快照落库(SaveLayout),设备名/序列号反映安装器视角。
