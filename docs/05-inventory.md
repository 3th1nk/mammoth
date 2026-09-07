# 05 · 盘查体系(Inventory)

盘查回答两个问题:**这台机器有什么硬件?盘上现在是什么布局?**
前者决定安装意图怎么写(选哪块盘、配哪块网卡),后者决定"保留数据"类意图是否可行。

## 1. 三探针体系

| 探针 | 通路 | 粒度 | 前提 | 定位 |
|------|------|------|------|------|
| `redfish` | BMC HTTPS(带外) | 硬件规格:盘/RAID 卷/网卡/CPU/内存/序列号/固件 | 机器支持 Redfish(2015 年后主流机型基本具备) | **默认**;同时验证 BMC 凭证与连通性 |
| `inband_ssh` | SSH 到现存系统(带内) | **分区布局**:分区表/文件系统/挂载点 | 用户提供了 SSH 凭证且带内可达 | **"复用已有分区"意图的数据基础** |
| `ramdisk`(可选) | 通用内存系统,引导一次 | 分区布局 | 引导通道(PXE 或虚拟介质) | SSH 凭证拿不到时的兜底 |

`probe: auto` 策略下的组合逻辑:

```
redfish(必做:规格 + BMC 连通性验证)
  └─ 若 spec/请求含 ssh 凭证且带内可达 → inband_ssh(layout 快照)
      └─ 不可达且任务需要 layout → 提示注册 ramdisk 探针(若启用)
```

## 2. redfish 探针

采集内容映射:

| Redfish 资源 | 归集到 |
|--------------|--------|
| `/Systems/{id}/Storage` → `Drives[]` | `hardware.disks`(型号/序列号/容量/介质类型/协议) |
| 同上 → `Volumes[]` | RAID 逻辑卷(呈现规则:有卷呈卷,无卷呈物理盘) |
| `/Systems/{id}/EthernetInterfaces` + `NetworkAdapters/NetworkPorts` | `hardware.nics`(MAC/速率/PCI 地址/链路态) |
| `/Systems/{id}/Processor` / `Memory` | CPU/内存 |
| `/Managers/{id}` | BMC 型号/固件版本 |

工程要点:

- **呈现规则**:操作系统看到的是逻辑卷(RAID 场景)而非物理盘。Redfish 探针按
  "存在 Volumes 则呈现卷,否则呈现物理盘"的规则归集,保证用户在盘查结果里看到的
  集合与安装器最终看到的集合一致;
- BMC 通常不列 USB/SD 等可移除介质——这是行为差异而非缺陷,`hardware.disks` 以
  `removable` 标注来源判断;
- 已知盲区(老机型 Redfish 残缺、部分 HBA 背板枚举不全)通过**厂商兼容矩阵**管理
  (见 [07-bmc.md](07-bmc.md)),命中已知盲区时在盘查结果中标注 `coverage: partial`。

## 3. inband_ssh 探针

设计为**无代理、只读、一次性**:

```
ssh <machine> — 执行只读命令集(单次连接,超时短):
  lsblk -J -b -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINT,PKNAME,PARTLABEL,PARTUUID
  blkid -o export
  ip -j link
```

- 不向目标机写入任何文件;不改任何状态;失败即失败,无残留;
- 采集结果解析为 layout 快照(见 [04-install-spec.md](04-install-spec.md) §3),
  `source: inband_ssh`,`captured_at` 取采集时刻;
- 网卡链路信息(MAC/速率)同时刷新,供 network spec 的 `match` 使用。

**明确的能力边界(写入 API 文档与错误码)**:
机器带内不可达时,**分区级布局物理上不可得**——不存在任何带外通路能读取分区表。
此时"保留数据"意图只能退化为 `keep: disk`(整盘保留,按硬件序列号锚定)。
引擎不承诺第三种可能,API 校验层直接拒绝"带内不可达 + keep: partitions"的组合。

## 4. ramdisk 探针(可选启用)

- 通用内存系统,**按架构常驻两份**(amd64/arm64),不为机器定制;
- 个性化(回传地址、任务标识)经内核启动参数注入;
- 引导通道优先 PXE;无 PXE 环境时退化为"发行版安装器本身"(见 §6 与
  [06-install-pipeline.md](06-install-pipeline.md) 的 %pre 校验,二者共享校验逻辑);
- 采集完成回报后立即断电,不留驻。

## 5. 快照生命周期

```
注册机器 ──▶ 自动盘查(auto)──▶ layout v1(captured_at)
   │
   └─ 用户可随时 POST /machines/{id}/actions {"type":"discover"} 刷新
      → 生成 v2(追加,不改 v1)

install job 提交:
   preserve 声明 ──绑定──▶ 当前最新快照版本
   执行时 %pre 强校验(见 06)──▶ 漂移则 LAYOUT_DRIFT 中止
```

时效原则:**快照只表达"采集时刻的事实",不承诺"现在的事实"**。
所有依赖快照的执行路径必须有现场校验;UI/客户端应展示 `captured_at` 引导用户刷新。

## 6. 探针选择矩阵

| 场景 | 可用探针 | 结果粒度 |
|------|---------|---------|
| 全新裸机(无系统) | redfish | 规格级(本就无布局可言) |
| 重装·现存系统健康,已提供 SSH 凭证 | redfish + inband_ssh | 规格 + **分区级** |
| 重装·带内不可达(系统损坏/凭证失效) | redfish | 规格级(仅支持 keep: disk) |
| 以上 + 需要分区级但无 SSH 凭证 | redfish + ramdisk(若启用) | 规格 + 分区级 |
