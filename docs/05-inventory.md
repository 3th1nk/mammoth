# 05 · 盘查体系(Inventory)

盘查回答两个问题:**这台机器有什么硬件?盘上现在是什么布局?**
前者决定安装意图怎么写(选哪块盘、配哪块网卡),后者决定"保留数据"类意图是否可行。

## 1. 三探针体系

| 探针 | 通路 | 粒度 | 前提 | 定位 |
|------|------|------|------|------|
| `redfish` | BMC HTTPS(带外) | 硬件规格:盘/RAID 卷/网卡/CPU/内存/序列号/固件 | 机器支持 Redfish(2015 年后主流机型基本具备) | **默认**;同时验证 BMC 凭证与连通性 |
| `inband_ssh` | SSH 到现存系统(带内) | **分区布局**:分区表/文件系统/挂载点 | 用户提供了 SSH 凭证且带内可达 | **"复用已有分区"意图的数据基础** |
| `ramdisk`(可选,`MAMMOTH_RAMDISK_ENABLED` 门控) | alpine 微型 live 环境,引导一次 | 分区布局 | BMC 虚拟介质(V1 已通;PXE 后续) | SSH 凭证拿不到时的兜底;载体 ISO 由 `MAMMOTH_PROBE_ALPINE_ISO` 声明 |

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
  lsblk -J -b -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINT,PKNAME,PARTLABEL,PARTUUID,SERIAL,PTTYPE
  blkid -o export
  ip -j link
  /sys/block/*/*/start          # sysfs 起始扇区(lsblk 不暴露,契约要求 start_bytes)
  安装器运行时标记探测(/run/anaconda 等 → installer_env,verify_ready 防假阳性用)
```

- 不向目标机写入任何文件;不改任何状态;失败即失败,无残留;
- 采集结果解析为 layout 快照(见 [04-install-spec.md](04-install-spec.md) §3),
  `source: inband_ssh`,`captured_at` 取采集时刻;
- 网卡链路信息(MAC/链路态)同时刷新,供 network spec 的 `match` 使用;
- 凭证支持私钥(推荐——Rocky 9 默认 `PermitRootLogin prohibit-password` 拒密码)
  与密码两种形态;失败分类为 `NETWORK_UNREACHABLE` / `CREDENTIAL_AUTH_FAILED`。

**明确的能力边界(写入 API 文档与错误码)**:
机器带内不可达时,**分区级布局物理上不可得**——不存在任何带外通路能读取分区表。
此时"保留数据"意图只能退化为 `keep: disk`(整盘保留,按硬件序列号锚定)。
引擎不承诺第三种可能,API 校验层直接拒绝"带内不可达 + keep: partitions"的组合。

## 4. ramdisk 探针(可选启用)

- **V1 载体 = alpine standard 虚拟介质**(2026-09-13,qemu BIOS+UEFI 双模式
  闭环,复盘见 compat/huawei.md):任务级 `probe-<token>.iso`,builder 全量
  重打包 alpine 载体(与安装介质同机制),探针逻辑以 apkovl 覆盖层注入
  (local.d:纯 /sys 扫描 → 逐网口 udhcpc → wget 上报 → poweroff,
  引导到上报 ~1 分钟);
- 上报端点 `POST /render/{token}/probe-report`(机器面,token 即凭证),内容
  原样落库为 layout 快照(source=ramdisk,与 inband_ssh 快照同形);discover
  任务轮询快照水位,收到即弹出介质并断电(补偿);等待预算
  `MAMMOTH_PROBE_WAIT`(默认 10m),特性整体由 `MAMMOTH_RAMDISK_ENABLED` 门控;
- 个性化(上报 URL 内嵌任务 token)构建期烘入,无运行时配置;
- 载体 ISO 经 `MAMMOTH_PROBE_ALPINE_ISO` 声明(本地路径或 URL):**须 lts
  内核的 standard 版**——lts + modloop 才带全量真机存储驱动(megaraid_sas
  等);virt 内核/40MB 级 flavor 缺驱动,真机看不到盘;
- 网络策略:DHCP 优先(业界先例 Ironic/Tinkerbell 的 agent 同此);
  **无 DHCP 机房的静态兜底**,IP 数据源分两级——机器注册的 `ssh.address`
  为权威(mammoth 的带内路径即它的回程;支持 CIDR 形态,裸 IP 按
  `MAMMOTH_PROBE_PREFIX` 补前缀,默认 /24),无 ssh.address 的机器退到
  全局 `MAMMOTH_PROBE_STATIC_CIDR`;上报目标跨网段时由
  `MAMMOTH_PROBE_GATEWAY` 提供默认路由(同网段可空);
- PXE 形态("通用内存系统按架构常驻")保留为后续演进:虚拟介质按任务构建
  已满足当前盘查需求;无 PXE 环境时的另一兜底是"发行版安装器本身"
  (见 §6 与 [06-install-pipeline.md](06-install-pipeline.md) 的 %pre 校验,
  二者共享校验逻辑)。

## 5. 快照生命周期

```
注册机器 ──▶ 自动盘查(auto)──▶ layout v1(captured_at)
   │
   ├─ 用户可随时 POST /machines/{id}/actions {"type":"discover"} 刷新
   │  → 生成 v2(追加,不改 v1)
   └─ install job 成功后 verify_ready 自动刷新 ──▶ 装后视角快照
      (设备名 + serial 即安装器所见;Redfish 卷名 → /dev/sda 的鸿沟
       在下一轮重装时直接消除)

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
