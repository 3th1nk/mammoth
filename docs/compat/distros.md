# 发行版支持矩阵(distro support matrix)

运行时事实源是 `GET /api/v1` 的 `distros` 字段(驱动注册表实时导出);
本页是与之同步的人读版与各发行版的实现注记。语义见
[06-install-pipeline.md](../06-install-pipeline.md) §5。

| 发行版 | 驱动 | 安装器 | 应答文件 | 保留分区 | 网络稳定选择器 | 状态 |
|--------|------|--------|----------|----------|----------------|------|
| Rocky/RHEL 系(Rocky 9/Alma 9) | `rocky9` | Anaconda | kickstart(`inst.ks=`) | **full**:`%pre` 漂移守卫 + `--onpart/--noformat` | MAC → 接口名在 `%pre` 安装期解析 | ✅ v0.1 |
| Ubuntu Server 22.04 | `ubuntu22` | Subiquity | autoinstall(nocloud seed) | **partial**:`keep: disk` 可用;`keep: partitions/preserve` 提交即拒绝 | netplan `match.macaddress` 原生支持 | ✅ v0.3 |
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
    {"name": "ubuntu22", "keep_partition_support": "partial"}
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

- 应答文件:nocloud seed(`meta-data` + `user-data`,经 `ds=nocloud-net;s=<seed>/`);
- 网络:netplan(cloud-init network-config v2),bond slaves 用 `match.macaddress`;
- 存储:curtin storage config(disk→partition→format→mount 链);
  keep: disk = 该盘不进入 curtin 配置;
- 完成回报:`late-commands` 末尾 curl 完成回调;
- root 口令:user-data `chpasswd`(与 kickstart `rootpw` 同语义,一次性随机)。

### 新增发行版

实现 `render.OSDriver`(六个方法)+ 注册一行,编排层零改动:
`Registry.For(distro)` 路由、`KeepPartitionSupport()` 进入提交门禁与矩阵导出。
