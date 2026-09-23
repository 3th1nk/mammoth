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
| OEM 专有虚拟介质动作(无标准动作) | Huawei VmmControl(仅 NFS/CIFS);H3C HDM 同形动作(载荷同形,OEM 命名空间 `/Oem/Public/`,文档推演待真机,见 [h3c.md](h3c.md)) | ✅ 已实现:OEM 回退 + 任务轮询 + 应用内重试;OEM 路径按槽位资源 Actions 通告动态发现,未通告时按候选序(/Oem/Huawei/ → /Oem/Public/)试探;任务终态走黑名单判定(HDM 的 `Mounting` 等在途态不再误判) |
| Image URL 缺 TransferProtocolType 被拒(部分 BMC 不在错误里带 RelatedProperties) | Cisco C845A / OpenBMC(sushy `is_transfer_protocol_required`) | ⚠️ 预警:当前媒体 URI 走 nfs://(scheme 即协议),不触发;引入 HTTP 直链介质时需补参数重发 |
| RAID 建卷前须清外来配置(foreign config),作业分 real-time 与重启级 | Dell iDRAC(drac OEM) | ⚠️ 预警:`configure_raid` 阶段 Redfish Volume 创建已实现,遇 iDRAC 需 OEM 清配置 + 作业等待;华为卷名忽略已处理(按重扫绑定) |
| 会话创建速率限制(连发 2 个即 400)/并发 session 数受限 | **Huawei iBMC 6.41(2026-09-19 实测)**、Supermicro(会话数)、通用 | ✅ 已吸收:redfish 会话按 addr+user 缓存(TTL 5min),认证类失败失效重连;registry 同址串行天然限流 |
| reboot 后立即读电源状态拿到旧值 | 通用(power.py 等待 15s) | ✅ 等价:verify_ready/probe 轮询语义天然覆盖 |

上游参考:Ironic `ironic/drivers/modules/redfish/`、sushy `connector.py`
(ETag/重试)、`virtual_media.py`(PATCH 插入/协议探测)及各 bug/story
引用(bug 2031595、story 2008504)。

## 三大 BMC 适配面清单(iDRAC / iLO / iBMC;竞品经验 + 预检项)

引擎的厂商适配形态:**单 redfish 驱动 + OEM 内联回退 + 可选能力接口 + 本矩阵**
——不做 per-vendor driver 拆分(Ironic 按 WS-MAN/Redfish 协议分裂是历史成因;
mammoth redfish-only,回退内联即可)。适配面按七张面枚举,每面标注三大厂商
已知形态(来源:Ironic drac/ilo 模块、sushy、MAAS power drivers、实测):

| 适配面 | Huawei iBMC(实测✅) | Dell iDRAC(到货预检) | HPE iLO(到货预检) |
|--------|----------------------|----------------------|---------------------|
| 会话 | 速率限制→会话缓存(✅ 4c116ba) | 会话数上限~6;OEM LSOM… 标准 POST 即可 | 并发受限;iLO4 Redfish 弱,备 IPMI 路径 |
| 电源 | ResetType 子集预读+回退(✅) | ForceRestart 一般可用;iDRAC 自身卡死走 Manager.Reset | **POST 期间拒管理操作**(先 ForceOff 再配置);ResetType 子集预读 |
| 引导 | Boot PATCH 带 ETag(✅) | BootSourceOverrideMode(UEFI/Legacy)必须显式;持久 vs 一次性语义差异 | Once 支持 ✓;注意 OverrideEnabled 取值形态 |
| 虚拟介质 | 标准 → OEM VmmControl(NFS/CIFS)回退(✅);高频挂载劣化→Manager.Reset 恢复(✅) | 实时挂载可用,但"引导到介质"在部分固件需 LC 作业;Eject+insert 间 500(已吸收重试) | InsertMedia 走 URI 直拉;旧 iLO4 走 OEM;槽位枚举照常 |
| BIOS 写 | /Bios/Settings + 当前 ETag + 显式 Content-Type(✅ 4c116ba) | **LC 作业形态**:BIOS 属性写生成调度作业,重启后应用,作业需轮询(非 Redfish task)——BiosSetter 的 202 轮询路径预计要 OEM 作业扩展 | PATCH 属性表即可;同样 POST 期间被拒 |
| 盘/RAID | DriveID OEM 建卷回退(✅);无 SecureErase 动作(✅ 如实不支持) | **外来配置清理 + LC 作业**(预警表已列);CSIOR 需开启否则盘查残缺 | HPE SmartArray 走 OEM;标准 Volume 少见 |
| 盘查容忍 | ProcessorId 数字类型违规宽容(✅) | 一般规范 | 一般规范 |

预检动作(新厂商到货 30 分钟):跑 discover(盘查+固件采集)→ power
on/off → set_boot_device once → mount/eject → GET bios → GET drives,
逐面对照本表;现象不在表内即新增行。

### 固件底座视角:品牌之外的真正聚类

BMC 怪癖按**固件底座**聚类而非服务器品牌(Ironic/sushy 同款经验:同底座
怪癖跨品牌复现)。接触未知 BMC 时先判断底座,再对号入座:

| 固件底座 | 常见品牌 | 已知形态 | mammoth 状态 |
|----------|----------|----------|--------------|
| 自研闭环(iBMC/iDRAC/iLO) | 华为/超聚变、Dell、HPE | 见上表 | iBMC 实战闭环;iDRAC/iLO 清单就绪 |
| **AMI MegaRAC**(装机量最大 OEM 底座) | 超微、浪潮、华硕、**H3C(HDM,文档实证:响应头 `Server: AMI MegaRAC Redfish Service`,见 [h3c.md](h3c.md))**、大量信创整机(长城/宝德等) | 会话数限制、Redfish 完整度随代际浮动(x10/x11/x12)、IPMI 强 Redfish 弱的老机型;`/Systems/{id}/Storages` 非标准复数(iBMC 与 HDM 同形,gofish 按通告链接走通) | 预警表已有会话限制行(代表整个底座);IPMI 兜底路径可用;首个文档级样本(H3C HDM)佐证 OEM 动作/存储路径形态的底座共性 |
| **OpenBMC**(开源,增长中) | Meta/MSFT/IBM 主推,ASPEED 卡,IBM Power 全系 | virtual media 常 KVM-only 槽(预警已有);Host 管理接口部分非标;标准符合度偏好且迭代快 | 预警表已有;预计适配成本最低的底座 |
| 其他专用(ASMI/FSP、XCC、AMT、HMC) | IBM Power、Lenovo、Intel vPro、小型机 | 各自封闭生态 | 低频;预检动作通用 |

实际 encounter 概率分布(国内混合机群场景):iBMC 系 ≈ MegaRAC 系 > OpenBMC >
 长尾自研。任何未知 BMC 的答案都是同一句:跑一遍预检,对号入座。

上游参考补充:Ironic `ironic/drivers/drac.py` 与 `modules/drac/`(LC 作业、
CSIOR)、`modules/ilo/`(虚拟介质/POST 拒操作)、MAAS `src/provisioningserver/
drivers/power/`(电源驱动矩阵、power query 与部署解耦)。
