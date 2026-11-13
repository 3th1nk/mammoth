# 厂商实录：Dell iDRAC（R620/iDRAC7 2.65、R730/iDRAC8 2.82）

> 跨厂商共性见 patterns.md。本文是 Dell 的实测证据与 console 集成定案
> （2026-11-13/14 真机实录），实现落点 internal/bmcweb/dell + internal/bmcweb/avct。

## 实测环境

| | 机器 1 | 机器 2 |
|---|---|---|
| 机型 | PowerEdge R620（12G） | PowerEdge R730（13G） |
| BMC | iDRAC7 fw 2.65.65.65 | iDRAC8 fw 2.82.82.82 |
| Service Tag | CN7475144G0221 | CN7016357R00K4 |
| Redfish | 1.4.0 | 1.4.0 |
| 系统 | VMware ESXi 6.7.0（DCUI 在屏） | — |

机器形态同代跨两代固件，覆盖 12G/13G 兼容性下限。

## 发现与适配

### §1 Redfish 只读面（已适配，coverage: full）

仓库自带的 redfish 驱动（internal/bmc/redfish）在两台机器上全部只读方法
实测通过：Probe/PowerState/CollectInventory（coverage full）/Health/
SystemEventLog（500 条）/FirmwareInventory。iDRAC7（Redfish 1.4.0，OEM 极瘦，
仅 Jobs 资源）是兼容性下限——12G 无 DellManager OEM 资源。

### §2 修复定案：Storage 集合 Members 顺序不稳定（iDRAC8）✅ 已修

`firstVolumeStorage`（旧实现）取 `storages[0]`，而 **iDRAC8 的 Storage
集合 Members 顺序不稳定**（同一集合三次请求，RAID/AHCI 排位各不相同；
JSON 生成疑似 map 遍历序）。掷到 AHCI（无 Drives）即返回空盘列表。

修复（internal/bmc/redfish/volume.go，`walkStorages`）：遍历全部控制器；
CreateVolume 用**带 Volumes 集合**的那个（`volumeStorage`，找不到报
BMC_UNSUPPORTED）；PhysicalDrives **聚合**所有控制器的内联 Drives。
测试 volume_multi_test.go 用轮换 Members 的 fixture 钉死顺序无关性。
真机定案：R730 PhysicalDrives 从空列表恢复为 4 盘（连续多次调用稳定）。

**已知盲区（同次实测）**：该固件的盘资源 `SerialNumber` 字段为空字符串
（盘存在但 SN 不填）——CreateVolume 的成员按 serial 选择在 iDRAC8 上
不可用，直到 firmware 提供盘序列号；盘池枚举与容量不受影响。

### §3 静态 web 根损坏（R620/iDRAC7 2.65，影响走 web UI 的方案）

- 未认证访问任何路径 → 统一 302 → `/start.html`（连编造路径也 302）
- `/start.html`、`/login.html`、`functions.js` 等**静态资源一律 404**，
  Basic auth / 浏览器 UA / Referer / cookie 均不解锁
- `/data/*` 动态端点全部存活；`GET /index.html?ST1=..` 是动态端点（200）
- 操作员浏览器能看到 web UI 是**本地缓存**所致

结论：任何依赖 iDRAC web UI 页面的集成方案（代理 + 手动登录）在该机不可
用，除非 racadm racreset。引擎侧 console 桥只用动态 API，不受影响。

### §4 web 会话模型（/data API）

- **登录**：`POST /data/login`，form **必须 `user=` 在前 `password=` 在后**——
  iDRAC 按位置解析表单，`url.Values.Encode()` 的字母序（password 在前）
  会被当作"用户名=password"而报 authResult=1。响应 XML：
  `status/authResult/blockingTime/forwardUrl/errorMsg`，ST1/ST2 可能是独立
  元素也可能只在 forwardUrl（`index.html?ST1=..,ST2=..`）里。
- **ST2 头**：后续每个 /data 请求必须带 `ST2` 请求头（缺它 401，即使带
  session cookie）。ST1 用于命名 console JNLP。
- **登出**：`GET /data/logout`；302/401 视为已登出。
- **请求形态**：无 UA/Accept-Language 的裸请求对部分端点 404——客户端
  复刻浏览器头矩阵（internal/bmcweb/dell/console.go do()）。
- authResult：1=凭据错 4=权限 5=会话满；登录失败计数锁定，客户端禁止自动重试。
- TLS floor TLS 1.0 + 显式 legacy cipher suites（iDRAC6 族栈）。

### §5 viewer.jnlp（console 票据）

- URL：`GET /viewer.jnlp(<host>@0@<title quote_plus>@<unix_millis>@ST1=<st1>)`，
  带 session cookie + ST2 头 + Referer。注意 title 是
  `url.QueryEscape` 形态（空格→`+`）。
- 返回 `application/x-java-jnlp-file`，arguments：
  `ip/kmport/vport(=5900)/user=<num>@@<rand>/passwd=<rand>/apcp=1/version=2`
  —— **user/passwd 是 iDRAC 每次铸造的编码凭据**（会话级 ticket），不是
  明文。引擎侧拿到即用（见 §6），无需逆向其编码。

### §6 avct 虚拟控制台协议（端口 5900，APCP/TLS + AVSP）

iDRAC 6/7/8 的虚拟控制台是 Avocent 私有协议（viewer.jnlp 的
com.avocent.idrac.kvm.Main）。字节级规格：

**传输**：每条 TCP 连接先说 53 字节 APCP 预握手（`"APCP"` + u32 53 + u16
0x0100 + u16 0 + u8 type(3=控制/4=视频) + u8 2 + u8 34 + u8 0 + u32 caps(5)
+ u8 32 + 32B random），响应 caps bit0=明文 / bit2=升级 TLS（两台真机均
caps=4 → TLS 1.2 ECDHE）。协议版本 2.34。**控制口与视频口同端口 5900，
是两条独立 TCP/TLS 连接**（type3/type4 各一）。

**帧**：控制口 `"BEEF"` + u16 type + u16 总长（≥16 ≤32768）；视频口客户端
→服务器头 `00 00 00 00 01 <type> <len16>`；**视频口服务器→客户端头
`00 00 00 00 00 <type> <len16>`（无 BEEF 魔数）**——服务器的视频帧头与
控制口不同构，ReadFrame 按通道区分（transport.go）。

**登录**（0x0102）：`u8 ulen + 96 user + u8 plen + 96 pass + 8 RIP(0) +
u8 port + u8 channel + i32 clientRandom [+ u8 shareMode]`（209 body）。
port 字节 = KMPORT&0xFF（=12，不是直觉的 5900 低字节 0xDC——0x170C&0xFF
=0x0C=12，实测与 viewer 一致）。**两种凭据都可登录**：明文账号（direct）
或 viewer.jnlp 的编码 ticket（web 流程，§5）。登录响应 0x8305：
`status(0=OK) + applianceRandom + flags(bit0 视频走控制口, bit1 视频口直连
TLS, bit6 view-only) + msg`。

**视频通道**：登录响应 flags 决定通道；独立口时先发 Video Channel Auth
（`00 00 00 00 01 01 00 10` + clientRandom(与登录一致) + applianceRandom），
服务器回 **0x84 Connect Status（可能从任一 socket 到达，两个读循环都要
看）**，随后客户端发 0x030E VideoEnable + 0x0302 SetDisplayArea(1024x768)
+ 0x0304 Scale1to1。每 2s 控制口 KeepAlive 0x0400。视频流控：每 20 个需
Ack 包回 Video Ack（`BEEF 00 00 00 10` + 首字节 20）。

**视频编码**：帧魔数 `"VIDS"`（56 49 44 53）+ u16 type + u16 总长，负载以
`de ad be ef` 开头。类型：0x81 DVC15 / 0x82 DVC7 / 0x83 DVC7_GRAY /
0x8A DVC23（无损位流，命令循环 1xxxxxxx MakePixel、000/001/010 run、011
双色 series）、0x86 ASpeed JPEG（iDRAC7+ 有损全帧）、0x87/0x88/0x89 文本
模式/调色板/字体。**DVC15/23 的颜色序是 BGR**（R620 ESXi 黄条按 RGB 解出
青色，换序后正确——已真机定案）。实现见 dvc.go / aspeed.go / textmode.go。

## KVM 集成方案（✅ 已实现并通过真机 E2E，2026-11-14）

```
浏览器 noVNC ──websocket(RFB)──▶ 引擎 RFBBridge ──avct/APCP+TLS──▶ iDRAC :5900
                                   │ 内部：/data/login → viewer.jnlp 票据
                                   │       → 控制口登录(ticket) → 视频通道 → DVC 解码
                                   └ 出图/键鼠双向
```

- **统一入口不变**：`/machines/{id}/console` → `Supports()` 命中 dell
  launcher（vendor 含 "dell"，子串匹配）→ session 页 → BootScript 直接
  `location.href = '/bmcweb/<id>/kvm'`（不走 iDRAC web UI——见 §3）。
- **引擎拥有路由**：`/bmcweb/{id}/kvm`（noVNC 页）与 `/bmcweb/{id}/kvm/ws`
  （RFB websocket）在 NoRoute 里先于 proxy 拦截，gate cookie 绑定机器 id
  （跨机器 gate 拒绝）。vendored noVNC 1.5（ESM，MPL-2.0）走
  `/bmcweb/assets/novnc/`（go:embed）。
- **像素格式**：ServerInit 固定 32bpp RGBA；客户端 SetPixelFormat 读后
  忽略（RFB 规范允许服务器保留格式，noVNC 按 ServerInit 渲染）。
- **更新合并**：framebuffer 累积 damage，RFB 侧 40ms 周期 Drain 合帧，
  RAW rect 按 ServerInit 的 32bpp 发送（RFBBridge.updateLoop）。
- **会话生命周期**：一个 websocket = 一次 /data/login + 一条 5900 会话，
  关闭即 logout（防会话槽泄漏，对照 huawei.md §会话模型）。
- **virtual media 不在此通路**：走既有 Redfish MountMedia（driver 已有）。

E2E 实录（MAMMOTH_DELL_E2E='host=... user=... pass=...' go test
./internal/bmcweb/dell/ -run E2E）：登录 → JNLP ticket → 控制口登录
（flags=0x0012）→ 视频通道 → DVC15 解码 → framebuffer 99.8% 非黑，
导出 PNG 为完整的 ESXi DCUI 画面（色彩正确）。浏览器端经 IAB 实测：
session 页自动跳 noVNC，画面/键鼠可用。

**不采用**：
- 代理 + iDRAC web UI 手动登录（huawei 形态）：§3 静态根损坏且
  Java/JNLP 已无浏览器支持——仅作 generic fallback 的兜底形态。
- 逆向 ticket 编码自行构造凭据：viewer.jnlp 直接给现成 ticket，无需。
- iDRAC9（14G）Redfish VirtualMedia/HTML5 console 通路：无真机，留待。

## 遗留

- `.182`（13G/iDRAC8）的 console 桥未实测（登录端点形态按 §4 应同族）；
  `PhysicalDrives` 空 bug（§2）待修。
- DVC7 调色板色序、文本模式（BIOS POST 期）与 ASpeed JPEG 已实现但未在
  该机观察到位流（ESXi DCUI 只发 DVC15）；遇到实际机型再定案。
- 键鼠：RFB keysym→HID 映射覆盖 US 布局常用键（keymap.go）；pad 键/非 US
  布局按需补充。
