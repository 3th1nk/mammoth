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
必填——校验和是门禁不是元数据)→ fetch worker 串行拉取进内容寻址缓存
(`MediaDir/images/<sha256>.iso`,digest 重复的注册共享同一文件,下载中
失败/损坏一律不落缓存,事件 `image.ready`/`image.failed`)。安装 spec 的
`image.id` 引用注册件,提交时解析为缓存路径 + 注册 digest **快照进 job
spec**——此后注册件的删除不影响已提交作业;`image.checksum` 另行声明时
与注册 digest 不一致即 422。注册面不做 yum/apt 源站(那是另一个产品的
活),mammoth 只管"工件管理 + 源指配"。

## 5. Install Spec

安装意图的完整契约。`template` 持有它,job 可内联、可 `template_id` 引用 + 覆盖。

```jsonc
POST /api/v1/jobs
{
  "type": "install",
  "targets": {
    "machine_ids": ["mch_7f3a9c", "mch_8b2e01"],
    "overrides": {                        // per-machine 覆盖(与 spec 浅合并)
      "mch_7f3a9c": {
        "identity": { "hostname": "node-01" },
        "network": [ { "match": { "mac": "aa:bb:cc:dd:ee:01" },
                       "addresses": ["10.0.1.11/24"] } ]
      }
    }
  },
  "spec": {
    "image": {
      "source": "https://mirror.example/rocky9.iso",   // 与 id 二选一;file/http(s)/nfs URI
      "id": "img_01",                       // 工件库注册件(POST /images);与 source 互斥
      "checksum": "sha256:9f86d0...",       // id 引用时缺省取注册件 digest,冲突即拒
      "distro": "rocky9"                  // 显式声明;不做隐式探测(见 §6 取舍)
    },
    "storage":   { /* §5.1 */ },
    "network":   [ /* §5.2 */ ],
    "identity":  {
      "hostname_pattern": "node-{index}"  // 未逐台指定时的生成规则
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
      "mode": "802.3ad",                       // active-backup | 802.3ad | balance-tlb | ...
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
    "installer": "agent"     // setup | agent(缺省 = setup)。windows 专用;其他发行版提交即拒
  }  // boot.installer 可选:setup|agent|auto(缺省 setup;auto=按部署
  // 事实判定——SMB 导出已配置走 setup,未配置走 agent apply)
}
```

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
5. **异构批次的衔接 = overrides 收参数差异,分组多 job 收意图差异**:一份共享
   spec 只覆盖意图同构的批次。`targets.overrides` 浅合并是**顶层键整体替换**
   (`internal/api/handlers_jobs.go` mergeSpecOverride,零值检测防误擦)——
   storage/network/image 皆可逐机整段换,但 install 强制 base spec、列表字段
   不可"只加一条";IP/主机名/可被选择器吸收的盘差属参数级,进 overrides。
   RAID 布局/keep 语义/distro 不同属意图级差异,按等价类分组、一组一个 job——
   试算(逐台)resolved plan 相同者即一组,逐组独立 policy 本来就是需求。
6. **不引入 `[{targets, spec}]` 数组提交形态,组合逻辑留在客户端**:数组只买到
   一次 HTTP 往返 + 单一幂等键;若落成"单 job 多 spec 组",`job.spec` 审计面、
   `policy.abort_batch`、concurrency 等 job 级语义全部重定义,改动落在已冻结的
   v1 契约上。部分接受优于全有或全无:per-machine 无效片段创建时预失败该
   task、不阻塞兄弟机器。与"模板化复用由客户端自行管理"(docs/03-api.md §4)
   同一分工,薄封装先例 `internal/cli` / `scripts/acceptance.py`。若契约要动,
   优先级更高的是 overrides 合并语义的显式化(如深合并声明),属增量兼容。
