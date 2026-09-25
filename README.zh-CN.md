<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
    <img src="assets/brand/logo.svg" alt="mammoth logo" width="140">
  </picture>
</p>

# Mammoth

[![ci](https://github.com/3th1nk/mammoth/actions/workflows/ci.yml/badge.svg)](https://github.com/3th1nk/mammoth/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/3th1nk/mammoth)](https://github.com/3th1nk/mammoth/releases)
[![license](https://img.shields.io/github/license/3th1nk/mammoth)](LICENSE)

> 自包含的裸金属服务器安装引擎。
> 给它一个带外地址和一份凭证——或者让机器自己找上门——还你一台能跑起来的机器。

[**English**](README.md)

Mammoth 通过带外控制器(BMC)接管机器,自动盘查硬件与磁盘布局,并按声明式意图
安装/重装操作系统——同时把开关机、引导设备、虚拟介质等通用带外能力做成
一等公民 API。**纯后端,API-first;无内置 UI。**

- 设计文档:[docs/README.md](docs/README.md)
- API 契约(唯一事实源):[api/openapi.yaml](api/openapi.yaml)
- 状态与各版本变更:见 [Releases](https://github.com/3th1nk/mammoth/releases)
  与[路线图](docs/09-roadmap.md)。

## 为什么叫 Mammoth

英语里管"庞然大物般的苦役"叫 **a mammoth task**——给一批裸金属服务器装系统,
恰恰是运维圈公认的这种苦役。一台没有操作系统的服务器,像冰封在冻土里的猛犸;
但冰层下仍有心跳:BMC 那颗不依赖磁盘的带外芯片,整机"死"了也仍在搏动。
Mammoth 顺着这根心跳找到机器、盘点骨骼、听你声明它该成为的模样,然后独自
扛下所有重活——重打包 ISO、虚拟介质、DHCP/PXE 的二层博弈、四种安装器方言。
你只需要给它一个地址和一份凭证,它还你一台能跑起来的机器。

**The mammoth task, tamed.**

顶部那只披着长毛的 gopher 说的也是同一件事:小个子(单二进制、自包含),干重活。

## 一图看懂

**一条流水线,三条盘查通路,两种引导载体,四种安装器方言。**

```mermaid
flowchart TD
    subgraph ON["1 · 上电注册"]
        reg["注册机器:<br/>BMC 地址 + 凭证<br/>(Redfish,IPMI 兜底)"]
        zr["零注册:未知机器 PXE<br/>引导共享探针树<br/>→ pending_machines<br/>→ claim 升格注册"]
    end
    subgraph INV["2 · 盘查 / 探测"]
        rf["redfish(带外)"]
        ram["ramdisk 探针(alpine):<br/>PXE 或虚拟介质"]
        ish["inband_ssh 探针"]
    end
    subgraph INS["3 · 安装 —— 一份声明式 Install Spec"]
        vm["virtual_media(默认):<br/>BMC 挂载重打包引导 ISO<br/>(NFS/HTTP 介质仓库)"]
        pxe["pxe(可选):<br/>shim → grubnet → 内核<br/>DHCP/proxyDHCP + TFTP + HTTP"]
    end
    subgraph DIA["4 · 安装器方言"]
        ks["kickstart<br/>rocky · centos · kylin · UOS"]
        ai["autoinstall<br/>ubuntu 22.04 / 24.04"]
        ps["preseed<br/>debian 12 / 13"]
        wu["unattend<br/>windows 2019(UEFI-only)<br/>引导形态不同 ⤓ windows 通路"]
    end
    ver["5 · 校验:完成回调 + 带内 SSH<br/>探测(识别安装器)<br/>+ 装后布局快照"]
    reg --> rf
    zr --> ram
    rf --> vm
    ram --> vm
    vm --> ks
    vm --> ai
    vm --> ps
    pxe --> ks
    pxe --> ai
    pxe --> ps
    ks --> ver
    ai --> ver
    ps --> ver
    wu --> ver
```

### Windows 通路——windows 的引导形态不同

windows 的两条通路都不走常规内核 PXE 形态:setup 搭 **wimboot**
(HTTP 喂 bootmgfw/BCD/boot.sdi/boot.wim——plain iPXE,无 shim/grub),
agent 通路是**两次引导**(第二段重新武装同一个 wimboot 载体,让
`bcdboot`——而不是 Linux——写出 BCD)。

```mermaid
flowchart TD
    subgraph WS["windows 2019 · boot.installer"]
        st["**setup**(默认)<br/>PXE:plain iPXE → wimboot<br/>(bootmgfw · BCD · boot.sdi · boot.wim)<br/>安装源:部署层 SMB 导出<br/>vMedia:重打包 ISO + autounattend"]
        a1["**agent · 第一段**<br/>alpine agent → wimlib 直写<br/>install.wim 到 NTFS + in-wim<br/>注入 · ESP 留空"]
        a2["**agent · 第二段**<br/>重武装 wimboot WinPE →<br/>bcdboot 原生写出 BCD"]
    end
    st --> fbt["first boot:<br/>specialize/OOBE → 完成回调"]
    a1 -->|"applied → 重武装 PXE"| a2 --> fbt
```

### PXE 寻址:DHCP 决策框架

装机二层决定模式 —— **每部署声明一次,绝不在单次安装时猜测**
(双 DHCP 抢答在代码层无法根治):

```mermaid
flowchart TD
    q{"装机网段是否已有<br/>site DHCP 服务?"}
    q -- "没有" --> pool["**POOL 模式**<br/>配置 MAMMOTH_PXE_DHCP_POOL,<br/>mammoth 拥有该网段:<br/>· 为 PXE ROM 与安装器<br/>  应答 DHCP<br/>· arm 时预约地址:ping +<br/>  邻居表探测跳过被占<br/>  静态地址<br/>· 租约只发给已武装装机<br/>  任务的 MAC<br/>· 引导与目标系统使用<br/>  预约地址(ip= 参数)"]
    q -- "有" --> proxy["**PROXY 模式**<br/>不配置池,site DHCP<br/>拥有地址:<br/>· site DHCP 应答引导期 IP<br/>· mammoth 只附加 PXE<br/>  引导选项(67/4011)<br/>· 在 spec 里声明装机地址:<br/>  静态 ip= 参数 + netplan<br/>· verify 探测该声明地址"]
    pool --> vlan["**跨 VLAN**<br/>机器网段的 DHCP relay<br/>(ip helper)把广播转发给<br/>mammoth;应答按 giaddr<br/>回程(RFC 2131)。TFTP/HTTP<br/>是单播 —— NextServer 与<br/>介质/API 地址须从机器<br/>网段可达"]
    proxy --> vlan
```

### 发行版适配矩阵 —— 什么载体用什么镜像

| 发行版 | 方言 | virtual_media 镜像 | PXE 镜像 | PXE 安装源 | 真机 |
|---|---|---|---|---|---|
| rocky 9 | kickstart | minimal / DVD ISO(重打包) | 同 ISO(抽取引导文件) | HTTP 池或 NFS ISO | ✅ 双载体 |
| rocky 10 | kickstart | DVD ISO,UEFI-only 布局 | 同上 | 同上 | ✅ 真机闭环(2288H,UEFI-only) |
| centos 7 | kickstart | minimal ISO | 同 ISO | 同上 | ✅ 虚拟介质 |
| kylin V10 / V11 | kickstart | DVD ISO | 同 ISO | 同上 | ✅ V11 真机闭环 · V10 受限(静态网 NM) |
| UOS | kickstart | DVD ISO | 同源 ISO | NFS ISO | ✅ 双通路 |
| ubuntu 22.04 / 24.04 | autoinstall | **live-server** ISO(casper,重打包) | **live-server** ISO(squashfs 走 NFS) | 解包 ISO 树走 NFS | ✅ 双载体 |
| debian 12 / 13 | preseed | **netinst** ISO(重打包) | **netinst** ISO(签名 HTTP 池)**+ 官方 netboot.tar.gz** + 暂存 udebs | HTTP 池(校验和完整、by-hash 回填) | ✅ 双载体 |
| windows 2019 | unattend | 官方原盘重打包(媒体根 autounattend + SetupComplete wimlib 注入,UDF bridge) | setup:wimboot-over-PXE(SMB 安装源)· agent:两段式直写(HTTP win 树,无 SMB) | — | ✅ 真机六阶段全绿(setup + agent apply 双通路) |

经验法则:**netinst / minimal** = 小安装器自带软件池(PXE 友好);
**DVD** = 完全离线池;**live-server** = ubuntu 的安装器载体(casper);
**live desktop** = 不支持(内无安装器)。virtual_media 载体总是重打包
官方 ISO 并烘焙应答文件;PXE 载体抽取引导文件、把 ISO 内容当作包源。

### 回归基线 —— 主流服务器镜像

| 家族 | 基线镜像(最新点版本) | 载体覆盖 |
|---|---|---|
| RHEL 系 | Rocky 9.x minimal ISO | virtual_media ✅ · PXE ✅ |
| Ubuntu 系 | Ubuntu 22.04.5 与 24.04.x live-server ISO | virtual_media ✅ · PXE ✅ |
| Debian 系 | Debian 12 / 13 netinst ISO | virtual_media ✅ · PXE ✅ |
| 扩展 | Rocky 10(UEFI-only)· CentOS 7(legacy)· Kylin V10/V11 · UOS | 按需 |
| Windows 系 | Windows Server 2019(zh-CN MSDN) | PXE(wimboot + SMB 源)✅ 真机六阶段全绿 · virtual_media 🔧(被 iBMC 6.41 固件缺陷封死,UEFI-only,Standard Core) |

每轮回归:六阶段流水线全绿 → 无人值守首启 → 装机钥匙 SSH 探测。详见
[docs/runbooks/test-baselines.md](docs/runbooks/test-baselines.md)。

## 快速开始(一体化)

前置条件:Go ≥ 1.26,Docker(用于 PostgreSQL)。

```bash
docker run -d --name mammoth-pg -e POSTGRES_USER=mammoth -e POSTGRES_PASSWORD=mammoth \
  -e POSTGRES_DB=mammoth -p 5432:5432 postgres:16-alpine

go build -o bin/mammoth ./cmd/mammoth

MAMMOTH_DATABASE_URL='postgres://mammoth:mammoth@localhost:5432/mammoth?sslmode=disable' \
MAMMOTH_API_TOKEN=devtoken \
MAMMOTH_MASTER_KEY=$(openssl rand -base64 32) \
bin/mammoth serve --mode=all
```

配置采用 12-factor 环境变量(`MAMMOTH_*`)。裸二进制部署也可用 dotenv 文件——
已存在的环境变量优先于文件条目,非法值会在启动时报错而非静默默认。完整可用的
示例见 [deploy/mammoth.env.example](deploy/mammoth.env.example)(源自真实部署,
已脱敏)——拷贝后修改其中的密钥、地址与路径即可:

```bash
cp deploy/mammoth.env.example /etc/mammoth.env   # 修改密钥/IP/路径
bin/mammoth serve --env-file /etc/mammoth.env    # 或 MAMMOTH_ENV_FILE=...
```

试一下:

```bash
TOKEN='Authorization: Bearer devtoken'
# 1. 注册一份凭证(只写;永不回显)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"bmc","name":"demo","secret":{"username":"admin","password":"..."}}' \
  localhost:8080/api/v1/credentials
# 2. 注册一台机器(protocol=fake → 内置 BMC 模拟器)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"bmc":{"address":"fake://n1","protocol":"fake","credential_id":"cred_..."}}' \
  localhost:8080/api/v1/machines
# 3. 查看探测结果(注册即自动盘查;hardware=规格级,/layout=分区快照需带内通路)
curl -s -H "$TOKEN" localhost:8080/api/v1/machines/mch_...           # hardware / firmware / power_state
curl -s -H "$TOKEN" localhost:8080/api/v1/machines/mch_.../layout    # 最新布局快照(看 captured_at)
# 快照只表达采集时刻的事实——规划与执行间隔久了,先刷新再试算:
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"discover"}' localhost:8080/api/v1/machines/mch_.../actions
# 4. 试算安装方案(只读:select 解析到哪块盘、keep 是否命中快照,不烧引导)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"spec":{"image":{"distro":"rocky9"},"storage":{"disks":[{"select":{"match":{"size":"largest"}},"wipe":true}]}}}' \
  localhost:8080/api/v1/machines/mch_.../install-plan

# 5. 批量安装(完整 Install Spec:一份意图,按发行版落地方言)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "type": "install",
    "targets": {"machine_ids": ["mch_a", "mch_b"]},
    "spec": {
      "image":  {"source": "https://mirror.example/rocky9.iso", "distro": "rocky9"},
      "storage": {"disks": [{
          "select": {"match": {"type": "nvme", "size": "largest"}},
          "wipe": true,
          "partitions": [
            {"size": "512M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
            {"size": "1G",   "fs": "xfs",  "mount": "/boot"},
            {"size": "rest", "fs": "xfs",  "mount": "/"}]}]},
      "network": [{
          "match": {"mac": "aa:bb:cc:dd:ee:01"}, "set_name": "eno1",
          "addresses": ["172.16.1.11/24"],
          "routes": [{"to": "default", "via": "172.16.1.1"}],
          "nameservers": {"addresses": ["10.0.0.53"]}}],
      "identity": {"hostname_pattern": "node-{index}"},
      "access":   {"ssh_keys": ["ssh-ed25519 AAA you@host"]},
      "scripts":  [{"stage": "post_install", "content_base64": "ZWNobyBkb25lCg=="}],
      "boot":     {"strategy": "virtual_media"}
    },
    "policy": {"concurrency": 2, "on_task_failure": "continue"}
  }'
# → 202 + job_id;两台机器并发装机,单机失败不阻塞批次
# → root 密码缺省按任务随机,经 task.root_password 事件一次性投递
# → 数据盘保留:disks[].keep: disk|partitions + preserve(按快照分区号;
#   支持矩阵见 docs/06 §5,试算可预检 keep 是否命中)
# → bond/vlan、软件+硬件 RAID、pxe 载体等完整面见 docs/04

# 6. 开机/关机/挂载介质等通用带外动作(与安装解耦的一等公民 API)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"power_on"}' localhost:8080/api/v1/machines/mch_.../actions

# 7. 观察 job(事件流:SSE /api/v1/events;验收全流程:scripts/acceptance.py,
#    它扮演 fake 机器取应答文件并回报完成)
curl -s -H "$TOKEN" localhost:8080/api/v1/jobs/job_...
```

运行脚本化验收流程(含 SIGKILL → reaper → retry 证明):

```bash
MAMMOTH_FAKE_BMC_DELAY=8s bin/mammoth serve --mode=all &   # 慢速 fake BMC
python3 scripts/acceptance.py --api http://localhost:8080 --token devtoken \
  --dsn 'postgres://mammoth:mammoth@localhost:5432/mammoth?sslmode=disable'
```

## 一屏看懂架构

```
client ──▶ api (控制面, 无状态)                    ──┐
            runner (经 BMC 执行任务)               ──┼──▶ PostgreSQL (状态 + 表队列)
            builder (介质构建)                     ──┤
            prober (带内盘查)                      ──┘
```

- 单二进制多模式:`serve --mode=all|api|runner|builder|prober`
- 至少一次投递的任务队列 + 可见性超时(PG `SKIP LOCKED`);状态流转与队列操作共享同一事务域
- 每个任务都有心跳;reaper 将失联任务标记为 `interrupted`(可重试)——runner 崩溃也不会让任务永久挂起
- Redfish 优先,IPMI 兜底;`fake` 驱动用于开发与 CI
- 每次安装两条引导载体:BMC 虚拟介质(默认)或 PXE/iPXE 网络引导(内置 proxyDHCP + TFTP,Pixiecore 风格;经 `MAMMOTH_PXE_ENABLED` 开启——见 docs/operations.md §4.5)

## 交付物

| 镜像 | 基础镜像 | 模式 | 说明 |
|------|---------|------|------|
| `mammoth` | distroless(静态) | all / api / runner / prober | 无外部工具 |
| `mammoth-builder` | alpine + xorriso | builder | 特权,用于介质构建 |

```bash
docker compose -f deploy/compose.all-in-one.yml up -d   # 1×all + PG
docker compose -f deploy/compose.faceted.yml up -d      # 1×api + N×runner + 1×builder
```

受限网络:`--build-arg RUNTIME_IMAGE=<mirror>/distroless/static-debian12:nonroot`
与 `--build-arg GOPROXY=https://goproxy.cn,direct`。

## 官方控制台

不想敲 cURL?**[mammoth-console](https://github.com/3th1nk/mammoth-console)**
是本引擎的官方 Web 控制台——零私有后端的 Vue 3 单页应用:所有能力边界由
`GET /api/v1`(capabilities)驱动,所有动作都走你刚刚用过的同一套公开 API。

机器全生命周期(注册/自动盘查/电源/健康与 SEL/BIOS/擦盘,高危动作两段式确认)、
四步装机向导(install-plan 试算预览)、任务与日志实时流(SSE)、零注册入门、
事件审计与 HMAC 签名 webhook、⌘K 全局命令面板、暗色模式。`docker compose`
伴引擎部署,或 `npm run dev` 本地开发。

![Mammoth Console](https://github.com/3th1nk/mammoth-console/raw/main/docs/screenshots/machines.png)

## 开发

```bash
make build        # bin/mammoth
make test         # 单元测试(无外部服务)
make test-pg      # queue/store 契约测试套件,针对一次性 PG
make generate     # 从 api/openapi.yaml 重新生成(契约漂移会导致 CI 失败)
make acceptance   # 完整脚本化验收流程,针对本地 all-in-one
```

可观测性:JSON 日志带标准字段(`task_id` `machine_id` `job_id` `request_id` `stage`),
Prometheus 于 `/metrics`,OTel 边界埋点(默认 no-op,`MAMMOTH_OTEL_EXPORTER_ENDPOINT` 导出)。

## 许可证

Apache-2.0

> 注:`assets/win-apply/` 内含取自 Alpine Linux 软件源的 GPL-2.0+ 二进制
> (wimlib / mkntfs 及配套库),来源与许可证清单见
> [assets/win-apply/PROVENANCE.md](assets/win-apply/PROVENANCE.md)。

Apache-2.0

### Logo

mammoth gopher 是 **Renée French** 原作 Go gopher(CC BY 3.0)的衍生作品,
以相同许可证改编。见 `assets/brand/`。
