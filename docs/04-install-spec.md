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
      "source": "https://mirror.example/rocky9.iso",   // 或 "image_id": "img_01"
      "checksum": "sha256:9f86d0...",
      "distro": "rocky9"                  // 显式声明;不做隐式探测(见 §6 取舍)
    },
    "storage":   { /* §5.1 */ },
    "network":   [ /* §5.2 */ ],
    "identity":  {
      "hostname_pattern": "node-{index}"  // 未逐台指定时的生成规则
    },
    "access": {
      "root_password": "generate",        // generate | 显式值 | 缺省(禁用口令登录)
      "ssh_keys": ["ssh-ed25519 AAA..."]
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

## 6. 关键取舍

1. **`layout` 是 machine 的子资源而非独立实体**:快照时效性由 `captured_at` 表达,
   `preserve` 隐式绑定最新快照。要求用户手工管理快照 ID 会把易用性做砸;漂移由
   安装时强校验兜底(默认开启,`policy.verify_layout` 可关)。
2. **`select` 规则与序列号精确指定并存**:批量场景不可能逐台写序列号;
   `size: largest` 这类规则是 autoinstall / Ironic root device hints 验证过的批量写法。
3. **`distro` 显式声明**:从 ISO 自动探测发行版是易错的安全假象——同名镜像可能对应
   不同的内核参数集与应答文件方言。显式声明 + checksum 是底线。
4. **`root_password: generate` 为默认推荐**:一次性随机口令经任务事件一次性下发,
   避免批口令长期存续;`ssh_keys` 是更优路径,文档引导优先使用。
