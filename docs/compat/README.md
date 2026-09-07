# 厂商兼容矩阵(community compatibility matrix)

按厂商/机型记录 BMC 的已知行为差异与缺陷,运行时据此做**能力降级**与
**coverage 标注**(见 [07-bmc.md](../07-bmc.md) §4、[05-inventory.md](../05-inventory.md) §2)。

## 文件格式

每个厂商一个 YAML 文件,运行时通过 `MAMMOTH_COMPAT_DIR` 目录加载
(未配置时使用内嵌默认矩阵,当前为空——矩阵随社区报告增长):

```yaml
# docs/compat/<vendor>.yaml
vendor: supermicro          # 与 redfish Manufacturer 归一后小写匹配
models:                     # 可选;缺省匹配该厂商全部机型
  - "X12DP.*"
partial_inventory:          # 已知规格盘查盲区 → coverage: partial + 注记
  - "older BIOS: Storage collection missing drives behind HBA"
single_virtual_media: true  # 仅单槽介质(M6 双介质策略降级依据)
remote_uri_mount: false     # 不支持远程 URI 挂载,必须落盘
one_shot_boot: false        # 不支持一次性引导,补偿阶段需恢复引导顺序
known_defects:              # 固件版本区间 + 现象 + workaround
  - firmware: ">=1.0 <1.23"
    issue: "VirtualMedia.InsertMedia ignores write-protected flag"
    workaround: "retry after power cycle"
```

## 运行时语义

| 字段 | 生效点 | 阶段 |
|------|--------|------|
| `partial_inventory` | 盘查结果标注 `coverage: partial` + 注记 | M1 |
| `one_shot_boot: false` | 引导设置后任务补偿强制恢复引导顺序 | 安装流水线(M3) |
| `single_virtual_media` | 引导介质与发行版盘合并(单介质方案) | 安装流水线(M3) |
| `remote_uri_mount: false` | 介质先落盘再挂载 | 安装流水线(M3) |
| `known_defects` | 命中固件区间时按 workaround 提示 | 随对应能力 |

## 提交指引

1. 一个厂商一个文件,文件名 = 归一化厂商名(小写字母数字);
2. `models` 用 Go 正则语法,与 Redfish `Model` 属性做前后锚定匹配;
3. 每条 `known_defects` 必须带可复现的固件区间与 workaround;
4. PR 请附 Redfish 响应样本(可脱敏序列号/MAC)。
