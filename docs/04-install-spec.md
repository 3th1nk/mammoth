# 04 · 数据契约:资源与 Install Spec

> 本文定义核心资源的 schema 与安装意图(Install Spec)的完整契约。
> 字段命名 JSON snake_case;`jsonc` 注释仅用于文档,契约文件中不含注释。

## 1. machine

```jsonc
{
  "id": "mch_7f3a9c",
  "labels": { "env": "prod", "rack": "A3" },
  "bmc": {
    "address": "198.51.100.10",
    "protocol": "redfish",              // redfish | ipmi | auto(先 redfish,失败降级 ipmi)
    "credential_id": "cred_k2n9",
    "vendor": "dell",                    // 发现回填
    "model": "PowerEdge R750",
    "firmware_version": "7.10.30"
  },
  "hardware": {                          // redfish 探针回填;未盘查时为 null
    "serial_number": "J7QX123",
    "cpu": { "model": "...", "cores": 64 },
    "memory_bytes": 274877906944,
    "disks": [
      { "name": "nvme0n1", "serial": "S6XPN...", "size_bytes": 1920383410176,
        "medium": "ssd", "protocol": "nvme", "removable": false }
    ],
    "nics": [
      { "name": "eno1", "mac": "aa:bb:cc:dd:ee:01", "speed_mbps": 25000,
        "link_up": true, "pci_address": "0000:0b:00.0" }
    ]
  },
  "power_state": "on",                   // on | off | unknown
  "state": "ready",                      // registering | discovering | ready | error
  "last_error": null
}
```

`labels` 由用户自由使用(环境/机柜/归属),列表接口支持 `?labels=k=v` 过滤。

## 2. credential

```jsonc
POST /api/v1/credentials
{
  "type": "bmc",                         // bmc | ssh
  "name": "idrac-admin",
  "secret": { "username": "admin", "password": "..." }
}
// 201 {"id": "cred_k2n9", "name": "idrac-admin", ...}   — secret 不回显
```

## 3. layout(分区快照)

```jsonc
{
  "captured_at": "2026-09-07T08:00:00Z",
  "source": "inband_ssh",                // inband_ssh | ramdisk
  "disks": [
    {
      "device": "/dev/nvme0n1",
      "match": { "serial": "S6XPN..." },       // 与 hardware.disks 的关联键
      "table": "gpt",                          // gpt | mbr
      "partitions": [
        { "number": 1, "start_bytes": 1048576, "end_bytes": 537001472,
          "size_bytes": 536870912, "fstype": "vfat", "label": "EFI",
          "mountpoint": "/boot/efi", "uuid": "..." },
        { "number": 2, "size_bytes": 107374182400, "fstype": "xfs",
          "mountpoint": "/", "uuid": "..." }
      ]
    }
  ]
}
```

快照是**不可变记录**(追加新版本,不改旧版),`install-jobs` 的 `preserve` 声明隐式绑定
机器当前最新快照;校验机制见 [06-install-pipeline.md](06-install-pipeline.md)。

## 4. image / template

```jsonc
// image
{ "id": "img_01", "name": "rocky-9.4",
  "source": "https://mirror.example/rocky9.4.iso",
  "checksum": "sha256:9f86d0...", "distro": "rocky9", "size_bytes": 10200547328 }

// template:Install Spec 的可复用封装
{ "id": "tpl_std_rocky", "name": "standard rocky9 lvm",
  "spec": { /* 同 §5,不含 targets */ } }
```

### 4.1 工件库(image registry)

`POST /api/v1/images` 注册发行版 ISO:`source_url` + `sha256`(64 hex,
拉取完成后引擎自动嗅探 ISO 内容回填空缺的 `distro`/`version`——
纯 Go ISO 9660 解析(PVD+目录指纹:.treeinfo/.discinfo/.disk/info/sources/apks),
注册者自带的值优先;
**目录自动导入**:操作者直接拷入 `MediaDir/images` 的 ISO 由目录扫描器
(60s 周期)自动注册——内容寻址硬链接为 `<sha256>.iso` 规范名、嗅探
distro/version、来源记为 `dir://<文件名>`;文件被带外移除时条目转
`missing`(不自动删行,引用它的装机清晰失败),文件回归即恢复 ready;
scp 半程文件(修改时间在稳定窗口内)留待下一轮。库的 http(s) 注册通道
不受影响,且 `<sha>.iso` 预置文件免下载的语义(文件名即校验和)使目录
导入与 API 注册天然互通;
必填——校验和是门禁不是元数据)→ fetch worker 串行拉取进内容寻址缓存
(`MediaDir/images/<sha256>.iso`,digest 重复的注册共享同一文件,下载中
失败/损坏一律不落缓存,事件 `image.ready`/`image.failed`)。安装 spec 的
`image.id` 引用注册件,提交时解析为缓存路径 + 注册 digest **快照进 job
spec**——此后注册件的删除不影响已提交作业;`image.checksum` 另行声明时
与注册 digest 不一致即 422。注册面不做 yum/apt 源站(那是另一个产品的
活),mammoth 只管"工件管理 + 源指配"。

## 5. Install Spec

安装意图的完整契约。`template` 持有它,job 可内联、可 `template_id` 引用 + 覆盖。
spec 是**模板**:任意字符串值可携带 `{var:*}` 引用,提交时按台解析(§5.0)。

```jsonc
POST /api/v1/jobs
{
  "type": "install",
  "targets": {
    "targets": [
      // 逐台绑定:值差异(IP/主机名)走 vars;结构差异(某台盘布局不同)走 spec_patch
      { "machine_id": "mch_7f3a9c",
        "vars": { "ip_nic1": "10.0.1.11/24", "hostname": "node-01" } },
      { "machine_id": "mch_8b2e01",
        "vars": { "ip_nic1": "10.0.1.12/24", "hostname": "node-02" } }
    ]
  },
  "spec": {
    "image": {
      "source": "https://mirror.example/rocky9.iso",   // 与 id 二选一;file/http(s)/nfs URI
      "id": "img_01",                       // 工件库注册件(POST /images);与 source 互斥
      "checksum": "sha256:9f86d0...",       // id 引用时缺省取注册件 digest,冲突即拒
      "distro": "rocky9"                  // 显式声明;不做隐式探测(见 §6 取舍)
    },
    "storage":   { /* §5.1 */ },
    "network":   [ /* §5.2;地址可引用 {var:*},网关/DNS 批量共享 */ ],
    "identity":  {
      "hostname_pattern": "{var:hostname}"  // 也可 "node-{index}";显式逐台名 = 引用绑定变量
    },
    "access": {
      "root_password": "",                 // 缺省/空 = 自动生成;非空 = 明文口令
      "ssh_keys": ["ssh-ed25519 AAA..."],
      "capabilities": ["rdp", "winrm", "ping"]  // windows 专用,可选:装完后
      // 在防火墙额外启用的访问能力(rdp=远程桌面+放行,winrm=PowerShell 远程,
      // ping=回显请求放行);缺省不开——对全新安装放开入站管理是操作者的
      // 安全决策,按任务显式声明,未知值提交即拒
    },
    "scripts": [
      { "stage": "pre_install",  "content_base64": "..." },
      { "stage": "post_install", "url": "https://.../register.sh",
        "expected_exit_codes": [0] }
    ]
  },
  "policy": {
    "concurrency": 10,
    "on_task_failure": "continue",        // continue | abort_batch
    "verify_layout": true,                // 安装时强校验快照漂移
    "task_timeout_seconds": 3600
  }
}
```

### 5.0 targets — 批量绑定与变量模板

一次批量 = 一份 spec 模板 + 每台一张 `vars` 绑定表(Cobbler system/xCAT nodedef
的同形物,Tinkerbell hardware data 亦然):

```jsonc
{ "machine_id": "mch_7f3a9c",
  "vars":       { "ip_nic1": "10.0.1.11/24", "hostname": "node-01" },
  "spec_patch": { /* RFC 7386 深合并,仅限结构差异;null 删键;可缺省 */ } }
```

spec 模板里任意字符串值可携带引用,**提交时逐台解析**:

- `{var:name}` → 该台 `vars[name]`(缺失 → 422 `SCHEMA_UNRESOLVED_VAR`,点名机器+变量);
- 内建上下文:`{index}`(targets 数组 1 起序)、`{machine_id}`、`{serial}`、
  `{bmc_address}`、`{label:key}`(label 缺失同样 422);
- **字面量安全**:其余 `{...}` 一律原样保留——shell 的 `${...}`、awk 片段、JSON
  示例不受伤;`content_base64` 是不透明 blob,不做模板;
- 解析后校验:地址须为合法 IP/CIDR、网关/须为 IP、VLAN ID 须为正
  (422 `SCHEMA_INVALID_ADDRESS`,点名位置)——错字挡在提交期,不会装完才发现
  网络不通。

**解析管线**:合并 `spec_patch` → 变量替换 → 解析后校验 → 方言渲染。方言层
零感知,拿到的永远是解析后的 spec。`install-plan` 请求体 `{spec, vars}` 走同
一条管线——试算通过即安装可过,变量语义只在引擎实现一次。批量静态 IP 就是
这个机制的直接受益者:网关/DNS/匹配键共享一处修改,逐台 IP 各归各位。


### 5.1 storage — 三档语义

```jsonc
{
  "disks": [
    {   // ① 精准重建(wipe)
      "select": { "match": { "type": "nvme", "size": "largest" } },
      "wipe": true,
      "partitions": [
        { "size": "512M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"] },
        { "size": "1G",   "fs": "xfs",  "mount": "/boot" },
        { "size": "rest", "fs": "xfs",  "mount": "/" }
      ]
    },
    {   // ② 整盘保留(带内不可达时唯一可承诺的档位)
      "select": { "match": { "serial": "GIM256_2021..." } },
      "keep": "disk"
    },
    {   // ③ 分区级复用(依赖 layout 快照)
      "select": { "match": { "serial": "S6XPN..." } },
      "keep": "partitions",
      "preserve": [
        { "number": 2, "mount": "/", "fs": "xfs" }     // 复用:不格式化,按原 uuid 挂载
      ],
      "partitions": [                                  // 该盘其余空间重建
        { "size": "rest", "fs": "xfs", "mount": "/data" }
      ]
    }
  ],
  "root_device_hints": { "min_size_gb": 100 },   // select 消歧兜底,防误选可移除介质
  "alignment": "4k"
}
```

**select.match 规则字段**:`serial`(唯一精确)/ `type`(`ssd|hdd|nvme`)/ `size`(`largest|smallest`)/
`protocol` / `removable`。规则与精确可组合(如 `type: nvme, size: largest`)。

**size 语法**:`512M` `100G` 字节字符串,或 `rest`(每盘至多一个,校验层强制)。

**schema 校验层强制约束**:

- `keep` 磁盘不得与 `partitions` 同用;
- `preserve[].number` 必须能在机器 layout 快照中命中,否则提交即 `SCHEMA_INVALID_STORAGE`;
- `wipe: true` 的盘与 `keep` 盘不得重叠(select 解析后按 machine 校验);
- 批量目标中任一机器缺少 layout 快照而 spec 含 `keep: partitions` → 该任务在提交时即标记
  `LAYOUT_SNAPSHOT_REQUIRED`(不阻塞其他机器,见 policy)。

#### 5.1.1 storage.lvm — 声明式卷组(新建与重装保留)

LVM 在**安装期**由安装器落地(anaconda `volgroup/logvol`、curtin
`lvm_volgroup/lvm_partition`、partman lvm 配方)——BMC 无 LVM 概念,流水线不新增阶段。
每组两种模式:

```jsonc
"lvm": [
  {   // ① 新建:PV 整设备消费(隐式擦除,与 disks[]/raid 成员互斥,如同 raid members)
    "name": "vg0",
    "pvs": [ { "match": { "size": "largest" } },          // 选择器引盘…
             { "raid": "md0" } ],                          // …或整引 raid[] 卷(该卷不得自带 partitions)
    "logvols": [
      { "name": "root", "size": "rest", "fs": "xfs", "mount": "/" },  // rest = 占满 VG 剩余(每 VG 至多一个)
      { "name": "swap", "size": "4G", "fs": "swap", "mount": "swap" }
    ]
  },
  {   // ② 重装保留 keep:vg:整卷组原子采用——VG 与全部 LV 原样保留、不格式化
    "name": "vgdata",
    "keep": "vg",
    "preserve": [                                          // 可选(0..n):要自动挂回的既有 LV
      { "name": "lvdata", "mount": "/data", "fs": "xfs" }  // 挂载点禁 / 、/boot、/boot/efi
    ]
    // preserve 留空 = 纯保护墓碑:PV 盘登记消费表防选择器抢占 + 装后核对存活,不写 fstab
  }
]
```

**约束与语义**:

- VG/LV 名 `^[A-Za-z][A-Za-z0-9_.]{0,63}$`(**禁 `-`**——/dev/mapper 会把 `-` 双写,下游工具全部遭殃);
- 新建模式 `pvs`/`logvols` 必填;`keep: vg` 时两者必须为空,绑定事实来自 layout 快照
  (同 `keep: partitions` 依赖快照的契约):快照无 LVM 事实或 VG/LV 缺失 → `LAYOUT_VG_NOT_FOUND`;
- **单盘布局(分区切出 PV)**:PV 选择器**允许**命中被 `disks[]` 声明(带分区)的盘——
  此时 PV = 该盘声明分区之外的**剩余空间**(自动成为末位分区),VG 建在其上。典型形态:
  `[ESP 1G] [+ /boot(kickstart 族)] [swap]` + PV 拿 rest,`/` 落 LV。约束:①holder 的
  `disks[]` 声明**不得含 rest/grow 分区**(剩余空间归 PV)→ `SCHEMA_INVALID_STORAGE`;
  ②holder 必须确有分区声明(keep: disk 整盘保留无切出面);③每个 holder 的剩余空间
  只供一个 VG。未声明分区的 PV 盘维持**整盘消费**原语义(与 `disks[]` 互斥如旧);
- **新建 VG 名不得与机器既有 VG 同名**(快照有 LVM 事实时预检)→ `LAYOUT_VG_EXISTS`
  ——否则 anaconda/curtin 会在安装几分钟内报「vg 已存在」而失败;
- keep:vg 的 PV 盘(含分区级 PV 所在盘)在 verify_layout 登记消费表,`disks[]`/raid/
  新建 VG 选择器**抢不走**;不声明 keep:vg 的既有 VG 默认**不受影响**——其 PV 盘只要
  不被选择器命中就原样存活,装后 lvm2 自动激活,可手动挂载(debian 不装 lvm 时需先
  `apt install lvm2`);想要这份保护就显式 `keep: vg`;
- LV 带 `esp` flag 或挂 `/boot/efi` → 引擎层拒绝(ESP 不可能住 LVM);
- **`/` 落在 LV 上时 kickstart 族要求非 LV 的 `/boot` 分区**(anaconda 无法从 LVM 引导,
  渲染期拒绝);ubuntu/debian 无此要求(grub2 可读 LVM);boot drive 相应回落到 `/boot` 所在盘;
- 不支持 thin provisioning 与 percent sizing(curtin/partman 均无,跨方言对齐优先,后续项);
- **装后核验**(按 bond 同款口径):声明随任务上下文快照,verify_ready 带内比对
  `vgs/pvs/lvs` 实测(含 preserve 的挂载点)——缺 VG/缺已声明 LV/挂载点不符 →
  `INSTALL_LVM_MISMATCH`;采集不到 LVM 事实(无 lvm2/带内不可用)→ `verify_ready_degraded` 降级事件,
  未声明的既有 VG 不在核对范围。

**方言矩阵**(详见 §5.3):kickstart 全支持(含 keep:vg、raid 卷作 PV);autoinstall 拒
software-raid PV(curtin 无 md 配方),其余全支持;preseed 仅新建、单 VG 单 PV 且 PV 必须是
唯一安装目标(partman-auto 单配方),keep:vg 拒绝;windows/agent 整体拒绝。

### 5.2 network — 对齐 cloud-init network-config v2 形状

```jsonc
[
  {
    "match": { "mac": "aa:bb:cc:dd:ee:01" },  // mac | pci_address | name(稳定性递减)
    "set_name": "mgmt0",                       // 可选:渲染为稳定接口名
    "addresses": ["10.0.1.11/24"],
    "routes": [ { "to": "default", "via": "10.0.1.1" } ],
    "nameservers": { "addresses": ["10.0.0.53"], "search": ["corp.local"] },
    "mtu": 9000
  },
  {
    "bond": {
      "interfaces": [ { "match": { "mac": "aa:..01" } }, { "match": { "mac": "aa:..02" } } ],
      "mode": "802.3ad",                       // 内核名或数字别名 0-6,归一为内核名
      "params": { "miimon": 100, "lacp_rate": "fast" }
    },
    "addresses": ["172.16.1.11/24"],
    "routes": [ { "to": "default", "via": "172.16.1.1" } ]
  },
  {
    "vlan": { "id": 100, "link": "bond0" },
    "addresses": ["192.168.100.11/24"]
  }
]
```

**bond 语义**(渲染期校验,违规即 `RENDER_FAILED`):

- `mode` 接受内核名或数字别名 `balance-rr(0) | active-backup(1) | balance-xor(2) | broadcast(3) |
  802.3ad(4) | balance-tlb(5) | balance-alb(6)`,归一为内核名;未知值渲染期拒绝并回显取值表。
- bond 按**声明顺序**命名 `bond0`、`bond1`…(与条目在 network 数组中的位置无关),`vlan.link` 引用该名字。
- `params` 的规范词汇是内核名(`miimon`、`lacp_rate`、`xmit_hash_policy`…);cloud-init v2/netplan
  拼写(`mii-monitor-interval`、`lacp-rate`、`arp-ip-targets`…)同样接受,入口折叠为内核名。
  同一参数两种拼写同现 → 拒绝;未知键 → 拒绝(拼写错的参数下游是静默失效,这正是校验要拦的);
  多值目标(`arp-ip-targets`)写 JSON 数组,规范形态为空格分隔。
- 出口词汇按方言:**rocky9 系** `--bondopts` 用内核名(sysfs 直通);**ubuntu 系** netplan 形状
  出口时翻译为 netplan 拼写(netplan 解析表只认 netplan 名,内核名键会被静默丢弃)——netplan
  无法表达的内核专有参数(`use_carrier`、`ad_actor_*`、`tlb_dynamic_lb`)在 ubuntu 渲染期拒绝。
  kickstart `--bondopts` 逗号分隔选项,`arp_ip_target` 仅支持单目标,多目标渲染期拒绝。
- `params` 未声明链路监测时注入默认 `miimon: 100`——与内核 v4.18 起的自身默认一致
  (commit c1f897ce186a,条件同为未声明 `arp_interval`);显式渲染使产物不依赖运行内核
  年代(4.18 前默认 0 = 监测关闭,主备切换永不发生)。声明了 `arp_interval` 的按 ARP
  监测走,不注入(内核 ARP/MII 监测互斥,后设者顶掉先设者)。
- `802.3ad` 要求交换机侧配好 LACP 聚合口,否则装完不通;建议显式 `xmit_hash_policy`
  (内核缺省 layer2,跨层负载常见选 layer3+4)。
- slave 以 mac 选择,空选择器(如 match 未携带 mac)渲染期拒绝。
- **装后核验**:归一化 bond 声明随任务上下文快照,verify_ready 带内比对实测
  `/sys/class/net/*/bonding/*`(成员 MAC 集/mode/miimon/active-backup 的 active 成员),
  不符 → `INSTALL_BOND_MISMATCH`(docs/06-install-pipeline.md §4)。

接口选择优先级:**mac > pci_address > name**。MAC 与硬件一一对应,是批量场景唯一
跨发行版稳定的选择器;`name` 受发行版命名方案(eno/ens/eth)影响,仅在用户明确知道
目标命名时使用。**Mammoth 不做地址分配**——所有 `addresses` 由调用方显式给出。

### 5.3 发行版方言差异(提交可过、渲染期拒绝的项)

spec 是发行版无关的声明;方言不能落地的项在**渲染期显式拒绝**
(`RENDER_FAILED`,任务失败且错误文案带方言说明),不做静默降级:

| 项 | rocky9 | ubuntu22 | debian12 / uniontechos | windows |
|----|--------|----------|------------------------|---------|
| bond / vlan | ✅(`%pre` 解析) | ✅(netplan 原生) | ❌ netcfg 无 bond/vlan | ❌ |
| 多条静态接口 | ✅ | ✅ | ❌ netcfg 单接口(一条静态 + 其余 dhcp) | 单接口(MAC 绑定) |
| 软件 RAID | ✅(raid 行) | —(未接) | ❌ partman md 配方未接 | ❌ |
| 硬件 RAID 卷 | ✅(绑定设备) | ✅(serial 绑定) | ✅(绑定设备,单目标) | ❌ |
| LVM 新建 | ✅(volgroup/logvol) | ✅(curtin lvm_*) | ✅(单 VG 单 PV,`/` 在 LV 无需 /boot) | ❌ |
| LVM + 软件 RAID 叠加 | ✅(raid 卷作 PV) | ❌(curtin 无 md) | ❌(软件 RAID 本就拒绝) | ❌ |
| LVM keep:vg(重装保留) | ✅(`--useexisting`) | ✅(curtin preserve) | ❌ partman 无法采纳既有组 | ❌ |
| `/` 在 LV 需独立 /boot | ✅(anaconda 强制) | 不需要(grub2 读 LVM) | 不需要(grub2 读 LVM) | — |
| 多安装目标盘 | ✅ | ✅ | ❌ partman-auto 单盘(其余盘 keep: disk) | ❌ |
| xfs | ✅ | ✅(curtin) | ❌ netinst partman 白名单 ext2/3/4、vfat/fat32、swap | —(ntfs) |

**windows 的 scripts 运行座位**(shell 字段仅 windows 消费,Linux 恒 sh):

- **post_install = 引擎托管首启链**(两条通路统一):段内容随 task.json
  下发,引擎 `mammoth-complete.ps1` 在静态网绑定之后、完成回调之前逐段
  执行(`shell=cmd` 经 `cmd /d /c`,`shell=powershell` 经
  `powershell -File`,缺省 cmd);`expected_exit_codes` 在此被消费(首个
  失败段即停,剩余段跳过,回调发 `status=failed` + 段位/退出码 detail →
  任务 `INSTALL_FAILED`)——**回调永远收尾**,失败路由进回调而非绕过它;
  ps1 双执行(SetupComplete SYSTEM + FirstLogonCommands Administrator,
见 compat/distros.md §windows)由 `mammoth-scripts.done` 哨兵吸收:第二
  次执行不重跑段,重放已记录的判定,副作用脚本只执行一次;
- **pre_install = setup 通路 WinPE 座位**(仅 `boot.installer=setup`):
  内联内容写介质 `mammoth/pre-<n>.cmd`,`autounattend` windowsPE
  RunSynchronous 定位执行(盘符扫描,退出码即脚本退出码;非零 → setup
  中止,无回调 → 任务 `INSTALL_TIMEOUT`);**限 shell=cmd + inline**
  (WinPE 无 PowerShell;vMedia boot.wim 无取数器);`expected_exit_codes`
  忽略(与 Linux 安装器方言先例一致);
- **渲染期拒绝**:agent 通路的 pre_install(装前运行时是 busybox agent
  ——Linux 语义,非 WinPE)、pre_install 的 powershell/url 形态、未知
  shell 值。

### 5.4 boot — 载体与安装通路

```jsonc
{
  "boot": {
    "strategy": "pxe",       // virtual_media | pxe(缺省 = 部署默认,capabilities.boot_strategy_default)
    "installer": "agent",    // setup | agent(缺省 = setup)。windows 专用;其他发行版提交即拒
    "kernel_args": "nomodeset modprobe.blacklist=nouveau",
    "installed_kernel_args": "modprobe.blacklist=nouveau"
  }  // boot.installer 可选:setup|agent|auto(缺省 setup;auto=按部署
  // 事实判定——SMB 导出已配置走 setup,未配置走 agent apply)
}
```

`kernel_args` / `installed_kernel_args` — **内核参数逃生口**(无人值守装机的
"GRUB 按 e"替代)。真实场景:浪潮 NF5280M6 + Tesla T4 级 GPU,安装器内核的
nouveau 驱动初始化不兼容 GPU 即挂死;有人值守可按 `e` 手敲
`nomodeset modprobe.blacklist=nouveau`,无人值守只能任务静默超时。两字段的
生命周期:

| 字段 | 生效范围 | 落点 |
|------|---------|------|
| `kernel_args` | 仅安装器环境 | 引擎拼的引导行末尾(ISO 的 isolinux/grub 与 PXE netboot 项两条) |
| `installed_kernel_args` | 装好的系统 | kickstart `bootloader --append`;autoinstall/preseed 写 `GRUB_CMDLINE_LINUX` + update-grub;agent 通路的 grub.cfg 行 |

GPU 类问题两段都要:`kernel_args` 解安装阶段,`installed_kernel_args` 解
装好后的首启(nouveau 敌对 GPU 装完照样挂,直到装上厂商驱动)。

契约约束(提交即拒,`SCHEMA_INVALID_BOOT`):

- token 按空白切分,每个须匹配 `^[A-Za-z0-9._,:%/+=@^~-]+$`——同一字符串
  原样进 iPXE `kernel` 行与 GRUB `linux` 行,引号/`;`/`$` 一律不入;
- 管道自有 key 拒绝(`BOOTIF` `ip` `console` `modules` `inst.ks` `inst.repo`
  `inst.syslog` `inst.text` `auto` `priority` `preseed/file` `seedfrom` `ds`
  `file` `modloop` `alpine_repo` `apkovl` `mammoth_base`)——引擎已拼,
  重复只会遮蔽语义(console 走 `MAMMOTH_INSTALL_CONSOLE`);
- windows 两字段均拒:WinPE 引导的是 BCD store,没有内核命令行可挂。

排障:安装期参数已随引导行进 `/proc/cmdline`,autoinstall early-commands
会快照回传 task_logs(9abe7c8);装后参数写进 `/etc/default/grub` 与
grub.cfg,`cat /proc/cmdline` 可验。



`installer` 选择 **windows 安装通路**(docs/compat/distros.md §windows 通路路线):

| 值 | 通路 | 载体 | install 源 | 前置 |
|----|------|------|-----------|------|
| `setup`(默认) | setup.exe 黑盒,wimboot-over-PXE 或虚拟介质 | wimboot / 虚拟介质 CD | 部署层 SMB 导出 | `MAMMOTH_WINDOWS_INSTALL_SMB_UNC` |
| `agent` | agent apply-image:wimlib apply + 预烤 BCD + unattend 落 Panther,复用 Linux agent 引导与声明式落盘 | alpine agent(shim→grubnet,SB 链同构) | 引擎 HTTP 面(`/netboot/store/<sha>/win/tree/`),**无需 SMB** | `MAMMOTH_WINDOWS_APPLY_ALPINE_ISO`(extended ISO:python3/sfdisk/partx)+ PXE |

边界:agent 通路 PXE-only、UEFI-only、仅 inbox 驱动机型(wimlib 只铺文件,
不做驱动服务化)。两通路回调面/verify 语义完全一致,可按机型混用。

### 5.5 package_source — 装机后的软件源指配

```jsonc
{
  "package_source": {
    "repos": [
      { "name": "internal",                       // [A-Za-z0-9._-]+,成为文件名后缀
        "url": "http://mirrors.int/rocky9",       // http(s)
        "gpg_key_url": "http://mirrors.int/key.asc",
        "suite": "9.4",                           // apt 侧;缺省取发行版代号(jammy/noble/trixie…)
        "components": "main" }                    // apt 侧;缺省 main
    ]
  }
}
```

离线机房刚需:装机完成即指向内网源,不依赖厂商公网源。渲染按方言分型——

| 方言 | 落地形态 |
|------|----------|
| rocky9(anaconda) | 安装期 `repo --name=mammoth-<name> --baseurl=` 指令 + `%post` 落 `/etc/yum.repos.d/mammoth-<name>.repo`(带 key → `gpgcheck=1`;不带 → `gpgcheck=0`) |
| ubuntu22/24(autoinstall) | late-commands 落 deb822 `/etc/apt/sources.list.d/mammoth-<name>.sources`;key 由安装器环境 python3 取到 `/target/usr/share/keyrings/`,`Signed-By` 指认;无 key → `Trusted: yes` |
| debian13(d-i) | post-install 脚本落 `/target/etc/apt/sources.list.d/mammoth-<name>.list`;key 由 busybox wget 取到目标 keyring,`signed-by` 内联;无 key → `trusted=yes` |
| windows | **渲染即拒**——无 yum/apt 语义,repo 声明写进 post_install 脚本 |

信任是显式的:声明了 `gpg_key_url` 的 repo 渲染签名校验,没声明的渲染
trusted(操作者故意为之,渲染文件如实记录)。提交期校验 name 字符集与
URL scheme——name 会成为配置文件名,字符集就是注入边界。

### 5.6 policy.health_gate — 装前硬件健康门禁

```jsonc
"policy": {
  "health_gate": "off"        // off | report | block(缺省 off)
}
```

固件门禁的同构延伸:坏盘在擦盘**之前**拦下,不是之后发现。数据源是
ramdisk 探针的快照——探针环境 best-effort 从引导介质自身 /apks 仓安装
smartmontools/nvme(载体带则装,不带则跳过,启动零风险),对每块盘采集
`smartctl -H` overall-health / NVMe `critical_warning`,快照 disks 项
携带 `"health": "pass" | "fail"`。

| 档位 | 行为 |
|------|------|
| `off`(默认) | 不检查 |
| `report` | install-plan 的 warnings 列出健康失败的盘(计划本就是提交前看的面);提交不拦 |
| `block` | 提交期拦截:最近快照存在 `health:"fail"` 即 422 `HEALTH_GATE_FAILED`(带 serial 名单);换盘或显式降档后重提 |

**只对有据可查的坏盘拦截**:快照缺失、载体没带工具、盘不报健康——一律
视为无数据,不拦不告。"无证据不发明结论"从探针采集(echo 空)到门禁
判定(只认显式 fail)贯穿同一条线。

## 6. 关键取舍

1. **`layout` 是 machine 的子资源而非独立实体**:快照时效性由 `captured_at` 表达,
   `preserve` 隐式绑定最新快照。要求用户手工管理快照 ID 会把易用性做砸;漂移由
   安装时强校验兜底(默认开启,`policy.verify_layout` 可关)。
2. **`select` 规则与序列号精确指定并存**:批量场景不可能逐台写序列号;
   `size: largest` 这类规则是 autoinstall / Ironic root device hints 验证过的批量写法。
3. **`distro` 显式声明**:从 ISO 自动探测发行版是易错的安全假象——同名镜像可能对应
   不同的内核参数集与应答文件方言。显式声明 + checksum 是底线。
4. **`root_password` 缺省/空 = 自动生成**:一次性随机口令经任务事件一次性下发,
   避免批口令长期存续;`ssh_keys` 是更优路径,文档引导优先使用。字符串形态下
   哨兵值就是空串(空口令本身非法),非空值一律视为明文,无控制值歧义。
5. **异构批次的衔接 = vars 收参数差异,spec_patch 收结构差异,分组多 job 收意图
   差异**:一份共享 spec 只覆盖意图同构的批次。IP/主机名/网关属参数级,进
   `targets[].vars`(§5.0);某台盘布局/keep 语义不同属结构级,进
   `targets[].spec_patch`(RFC 7386 深合并);distro 不同属意图级差异,按等价类
   分组、一组一个 job——试算(逐台)resolved plan 相同者即一组,逐组独立 policy
   本来就是需求。
6. **不引入 `[{targets, spec}]` 数组提交形态,组合逻辑留在客户端**:数组只买到
   一次 HTTP 往返 + 单一幂等键;若落成"单 job 多 spec 组",`job.spec` 审计面、
   `policy.abort_batch`、concurrency 等 job 级语义全部重定义。vars + spec_patch
   已覆盖参数级与结构级两类逐台差异,残余的意图级差异按 5 分组提交。与"模板化
   复用由客户端自行管理"(docs/03-api.md §4)同一分工,薄封装先例
   `internal/cli` / `scripts/acceptance.py`。
