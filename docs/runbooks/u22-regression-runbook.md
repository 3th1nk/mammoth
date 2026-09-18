# ubuntu22 真机回归操作清单

> **状态**：✅ 首轮回归已于 2026-09-12 真机通过（Huawei 2288H V5，端到端 ~3h，
> 见 [compat/huawei.md](../compat/huawei.md)）。本文档留作 ubuntu22 方言回归的
> 操作模板，后续复跑按此执行。
>
> 背景：首轮构建的引导介质曾不含应答文件（seed 取自渲染前的 task context），
> subiquity 拿不到 autoinstall 即进交互模式——卡语言选择。commit `1338a7b` 后
> seed 取自本轮渲染结果，本轮回归即验证该修复。

## 1. 启动 serve（真机参数）

```bash
export MAMMOTH_MASTER_KEY=$(openssl rand -base64 32)   # 每部署一次,持久保存
export MAMMOTH_DATABASE_URL="postgres://.../mammoth?sslmode=disable"
export MAMMOTH_EXTERNAL_URL="http://<mammoth主机IP>:8080"   # 安装器要回调 complete,必须机器可达
export MAMMOTH_API_TOKEN="devtoken"
# 介质导出:内置 NFS(默认开,2049)。BMC 挂载地址:
export MAMMOTH_MEDIA_BASE_URI="nfs://<mammoth主机IP>/data/media"
export MAMMOTH_MEDIA_WORKDIR="/var/tmp/mammoth-build"       # 可选:构建暂存(需 ≥2×ISO 空闲)
./bin/mammoth serve --mode=all
```

## 2. 镜像源（248 服务器）

`source` 二选一:
- **248 的 HTTP 地址**(EnsureISO 支持断点续传):
  `"source": "http://<248>/ubuntu-22.04.5-live-server-amd64.iso"`
- 或把 ISO 放进 mammoth 的 `MAMMOTH_MEDIA_DIR`(data/media/),source 用任意非 http 前缀:
  `"source": "nfs://cache/ubuntu-22.04.5-live-server-amd64.iso"`(按文件名命中缓存)

debian12 / uniontechos 同理(debian-12.x-amd64-netinst.iso / UOS V20 ISO)。

## 3. 注册与提交

```bash
# BMC 凭证
curl -X POST http://localhost:8080/api/v1/credentials -H "Authorization: Bearer $MAMMOTH_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"type":"bmc","name":"huawei-2288h","secret":{"username":"root","password":"<iBMC口令>"}}'

# 注册机器(TLS 自签名在凭证/环境变量里开 insecure)
curl -X POST http://localhost:8080/api/v1/machines -H "Authorization: Bearer $MAMMOTH_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"labels":{"env":"regression"},"bmc":{"address":"https://<iBMC地址>","protocol":"redfish","credential_id":"<cred_id>"}}'

# 提交 install(等 machine state=ready 后)
curl -X POST http://localhost:8080/api/v1/jobs -H "Authorization: Bearer $MAMMOTH_API_TOKEN" \
  -H "Content-Type: application/json" -d @- <<'EOF'
{"type":"install","targets":{"machine_ids":["<machine_id>"]},"spec":{
  "image":{"distro":"ubuntu22","source":"http://<248>/ubuntu-22.04.5-live-server-amd64.iso"},
  "identity":{"hostname":"u22-regression"},
  "storage":{"disks":[{"select":{"match":{"serial":"<系统盘序列号>"}},"wipe":true,
    "partitions":[
      {"size":"512M","fs":"vfat","mount":"/boot/efi","flags":["esp"]},
      {"size":"rest","fs":"ext4","mount":"/"}]}]},
  "network":[{"match":{"mac":"<管理口MAC>"},"addresses":["<安装期IP>/24"],
              "routes":[{"to":"default","via":"<网关>"}]}],
  "access":{"root_password":"<明文口令或留空自动生成>","ssh_keys":["ssh-ed25519 AAA ..."]}}}
EOF
```

## 4. 观察点(修复验证链)

1. **prepare_media 日志**:`answers rendered, boot media built files=2 distro=ubuntu22`
2. **产物核对**(修复的直接证据,构建完成即可查,不必等装机):
   ```bash
   xorriso -indev data/media/boot-<taskToken>.iso -find /user-data     # 必须 '/user-data'
   xorriso -indev data/media/boot-<taskToken>.iso -find /meta-data     # 必须 '/meta-data'
   ```
3. **BMC 控制台**:grub 1s 自动进 mammoth 条目 → casper 启动 → **subiquity 不再停在语言选择**,
   直接进自动安装(分区/网络/身份全部免交互)
4. **事件流**:task.root_password → stage 变更 → install_os(等待) → 完成回调 → verify_ready
5. **装后核对**:root 口令/ssh key 可登录、IP 与 spec 一致、/boot/efi+/ 布局正确

## 5. 若仍异常

- 控制台若仍停语言选择:导出 `data/media/boot-<token>.iso`,核对上述第 2 步;
  seed 在而仍卡 → 看 `/run/casper` 或 cloud-init 输出,可能是 `file:///cdrom/` 数据源形态问题
  (与 seed 时序是两个独立问题,反馈日志即可)
- 装后不可达:确认 spec 网络(静态 IP/MAC 钉口)与控制台实际一致;重装后目标机
  SSH host key 轮换属正常,登录端清旧记录即可

## 6. 方言矩阵现状

- **debian12**:✅ 2026-09-12 真机通过(netinst,~6min);注意
  `MAMMOTH_EXTERNAL_URL` 用 **http**(d-i busybox wget 的 TLS 受限);
- **uniontechos(UOS)**:✅ 2026-09-19 真机闭环(虚拟介质零人工六阶段绿)
  ——ISO 实测为 anaconda 定制(RHEL 系树,非 d-i),归 kickstart 方言;
  曾 blocked 的 Finish 崩溃已定位为 UOS 定制 anaconda 无 swap 崩溃并修复
  (根因全录见 [compat/distros.md](../compat/distros.md))。
