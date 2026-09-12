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

## 驱动层的跨厂商防御(借鉴自 Ironic / sushy)

Ironic 的 redfish 通用驱动与 sushy 库沉淀了十年厂商坑。mammoth 的驱动层
已吸收其中结构性模式(2026-09 调研对照),其余作为**预警清单**收录于此,
撞到对应现象时先查此表:

| 现象 | 厂商/来源 | mammoth 现状 |
|------|----------|--------------|
| 虚拟介质槽不支持 InsertMedia 动作(KVM-only/内部槽)或插入报 400/405 | Cisco UCS(内部槽)、OpenBMC(KVM-only 槽) | ✅ 已吸收:多槽位枚举 + 逐槽降级(`orderMediaSlots`),失败换下一槽 |
| 无 InsertMedia 动作但接受资源 PATCH(Image/Inserted) | sushy `_allow_patch()` 路径 | ✅ 已吸收:动作全部失败后按 PATCH 形态重试(`vm.Update()`) |
| 槽位只有 DVD 没有 CD | Cisco(bug 2031595) | ✅ 已吸收:槽位排序 CD→DVD→其余 |
| eject 刚发出,紧接的 insert 500 | Dell(story 2008504) | ✅ 已吸收:eject 失败重试一次(3s) |
| PATCH 主资源要求 If-Match(ETag),无 Settings 对象 | Huawei iBMC 6.41(实测 412) | ✅ 已吸收:Boot PATCH 携带资源自身 ETag(`patchSystemBoot`) |
| Reset 只接受标准值的子集 | Huawei iBMC(ForceOn/PowerCycle/GracefulRestart 被拒) | ✅ 已吸收:`ResetType@Redfish.AllowableValues` 预读 + 值映射回退 |
| OEM 专有虚拟介质动作(无标准动作) | Huawei VmmControl(仅 NFS/CIFS) | ✅ 已实现:OEM 回退 + 任务轮询 + 应用内重试;OEM 路径按槽位资源动态发现 |
| Image URL 缺 TransferProtocolType 被拒(部分 BMC 不在错误里带 RelatedProperties) | Cisco C845A / OpenBMC(sushy `is_transfer_protocol_required`) | ⚠️ 预警:当前媒体 URI 走 nfs://(scheme 即协议),不触发;引入 HTTP 直链介质时需补参数重发 |
| RAID 建卷前须清外来配置(foreign config),作业分 real-time 与重启级 | Dell iDRAC(drac OEM) | ⚠️ 预警:`configure_raid` 阶段 Redfish Volume 创建已实现,遇 iDRAC 需 OEM 清配置 + 作业等待;华为卷名忽略已处理(按重扫绑定) |
| 并发 session 数受限、认证失败后 session 不可复用 | Supermicro(会话数)、通用 | ⚠️ 预警:registry 已同址串行(天然限流);遇 AccessError 重建会话即可 |
| reboot 后立即读电源状态拿到旧值 | 通用(power.py 等待 15s) | ✅ 等价:verify_ready/probe 轮询语义天然覆盖 |

上游参考:Ironic `ironic/drivers/modules/redfish/`、sushy `connector.py`
(ETag/重试)、`virtual_media.py`(PATCH 插入/协议探测)及各 bug/story
引用(bug 2031595、story 2008504)。
