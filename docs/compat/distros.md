# 发行版支持矩阵(distro support matrix)

运行时事实源是 `GET /api/v1` 的 `distros` 字段(驱动注册表实时导出);
本页是与之同步的人读版与各发行版的实现注记。语义见
[06-install-pipeline.md](../06-install-pipeline.md) §5。

| 发行版 | 驱动 | 安装器 | 应答文件 | 保留分区 | 网络稳定选择器 | 状态 |
|--------|------|--------|----------|----------|----------------|------|
| Rocky/RHEL 系(Rocky 9/Alma 9) | `rocky9` | Anaconda | kickstart(`inst.ks=`) | **full**:`%pre` 漂移守卫 + `--onpart/--noformat` | MAC → 接口名在 `%pre` 安装期解析 | ✅ v0.1 |
| Ubuntu Server 22.04 | `ubuntu22` | Subiquity | autoinstall(nocloud seed) | **partial**:`keep: disk` 可用;`keep: partitions/preserve` 提交即拒绝 | netplan `match.macaddress` 原生支持 | ✅ v0.3 |
| Debian 12 | `debian12` | debian-installer | preseed(`file=/cdrom/preseed.cfg`) | **partial**:`keep: disk` 可用;`keep: partitions/preserve` 提交即拒绝 | 无(netcfg 不按 MAC 选口,单接口) | ✅ 真机跑通 |
| 统信服务器 V20(UOS) | `uniontechos` | **anaconda 定制**(RHEL 系安装树:AppStream/BaseOS/isolinux,非 d-i) | kickstart(同 `rocky9` 方言) | **full**(同 `rocky9`,待真机复核) | MAC → 接口名在 %pre 安装期解析 | experimental,真机联调中 |
| Windows | — | Setup | unattend | full(目标) | — | 未开始 |

## 保留分区支持语义(SupportLevel)

| 级别 | 提交侧行为 | 渲染侧行为 |
|------|-----------|-----------|
| `full` | keep: disk / keep: partitions / preserve 全部受理(仍受快照命中校验) | 完整渲染 + `%pre` 漂移守卫 |
| `partial` | keep: disk 受理;keep: partitions / preserve → `SCHEMA_UNSUPPORTED_KEEP` 拒绝 | keep: disk 以"不进入存储配置"实现;渲染层二次拒绝 keep: partitions |
| `none` | 一切 keep → `SCHEMA_UNSUPPORTED_KEEP` 拒绝 | — |

`GET /api/v1` 返回示例:

```json
{
  "version": "0.1.0",
  "resources": ["credentials", "machines", "jobs", "tasks"],
  "distros": [
    {"name": "rocky9",  "keep_partition_support": "full"},
    {"name": "ubuntu22", "keep_partition_support": "partial"},
    {"name": "debian12", "keep_partition_support": "partial"},
    {"name": "uniontechos", "keep_partition_support": "partial"}
  ]
}
```

## 实现注记

### rocky9(kickstart)

- 应答文件:`ks.cfg`(task-token URL 经 `inst.ks=` 注入);
- 网络:`%pre` 钩子运行时解析 MAC → 接口名,生成 `network` 行(bond slaves 按 MAC);
- 保留分区:`%pre` 漂移守卫逐分区比对 sysfs start/size 扇区与 blkid UUID,
  漂移即回报 `LAYOUT_DRIFT` 并终止安装;preserve 分区 `--onpart --noformat`;
- 完成回报:`%post` 末尾 curl 完成回调。

### ubuntu22(autoinstall)

- 应答文件:nocloud seed(`meta-data` + `user-data`,烘入 ISO 根,经
  `ds=nocloud-net;s=file:///cdrom/` 离线加载——远程 seed 依赖 casper 早期组网,
  真机已证不可靠);
- 网络:netplan(cloud-init network-config v2),bond slaves 用 `match.macaddress`;
- 存储:curtin storage config(disk→partition→format→mount 链);
  keep: disk = 该盘不进入 curtin 配置;
- 完成回报:`late-commands` 末尾 curl 完成回调;
- root 口令:user-data `chpasswd`(与 kickstart `rootpw` 同语义,一次性随机)。

### debian12 / uniontechos(preseed)

- 应答文件:`preseed.cfg` + `run/mammoth/{pre,post}-install.sh`,全部烘入 ISO 根,
  经 `file=/cdrom/preseed.cfg` 离线加载(d-i 将引导介质挂在 /cdrom);
  语言/键盘问题发生在 preseed 加载**之前**,以内核参数形式早期预置
  (`debian-installer/locale` / `keyboard-configuration/layoutcode`——d-i 内核参数
  天然是 preseed 键值);
- 介质形态:netinst 必须全量重打(d-i 从 ISO 的 pool//dists/ 读包),builder 探测
  `/install.amd`(兼容 `/install`)后走 casper 同款全量重打路径;
- 存储:`partman-auto/expert_recipe`(声明分区顺序即落盘顺序,esp → `method{ efi }`,
  swap → `method{ swap }`,grow → max=10⁹ MB 由 partman 截断);
  keep: disk = 该盘不成为 `partman-auto/disk` 目标;
- **方言限制(渲染期拒绝,表现为 RENDER_FAILED)**:bond/vlan/多条静态(netcfg 单接口
  且不按 MAC 选口)、软件 RAID(partman md 配方未接)、多安装目标盘(partman-auto 单盘)、
  xfs(netinst 是否携带 partman-xfs 未验证,白名单 ext2/3/4、vfat/fat32、swap);
- 完成回报:`preseed/late_command` 执行 `/cdrom/run/mammoth/post-install.sh`,
  busybox `wget --post-data` 上报(d-i 环境无 curl;**部署需 http**,busybox wget 的
  TLS 受限);pre_install 挂 EXIT failtrap,失败即回报阶段名;
- root 口令:`passwd/root-password` 明文(与 kickstart/ubuntu22 同语义,一次性随机);
- `uniontechos` 是同一方言的第二注册名(统信服务器 V20 的 d-i 定制安装器),
  真机验证内核目录名与 preseed 键兼容性后转正。

### ubuntu d-i(legacy) 支持决策:不主动支持

**决策(2026-09)**:不为 ubuntu 的 debian-installer 镜像(18.04 server ISO /
mini.iso)提供驱动支持。

- **上游已封存**:Ubuntu Server 自 20.04 起弃用 d-i,不再发布 d-i 版 ISO,
  mini.iso 构建同样停更;最后一个版本 18.04 标准支持已于 2023-05 EOL
  (仅 ESM 延续至 2028)。
- **20.04+ 无对应镜像**:ubuntu 批量装机的加速正道是 PXE + live(网络拉
  squashfs,较虚拟光驱快一个量级,roadmap M6),而非回退 d-i;ubuntu 方向
  优先投入 24.04 兼容性验证(subiquity/autoinstall v1 主线,预计现有驱动
  直接可用)。
- **重评估触发条件**:存量 18.04 机器出现批量纳管/重装需求时。

**如需适配的技术方向**(低成本,预计 ~1 天 + 一轮真机验证):

- **方言复用**:18.04 的 d-i 与 debian 12 同源——preseed 键、partman
  expert_recipe、`preseed/late_command`(busybox wget 回调)基本通用,即
  `debian.New("ubuntu1804")` 级别的变体注册,无需新驱动;
- **布局探测**:ubuntu d-i ISO 的内核目录为 `/install`(非 install.amd),
  `builder.debianInstallDir` 的候选列表已包含 ✅;注意 ubuntu live-server
  也含 `/install/`,探测顺序必须保持在 casper 之后(现有实现已如此);
- **需核对的差异点**:ubuntu 仓库/apt-setup 键的措辞差异、18.04 d-i 版本的
  partman 组件行为、ESM 源(若目标机依赖)的 mirror 配置;
- **风险**:绑定一个上游停止演进的安装器,后续无人修复——变体需在文档与
  capabilities 中如实标注支持边界。

### 新增发行版

实现 `render.OSDriver`(六个方法)+ 注册一行,编排层零改动:
`Registry.For(distro)` 路由、`KeepPartitionSupport()` 进入提交门禁与矩阵导出。
