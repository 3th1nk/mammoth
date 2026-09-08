# 厂商实录:Huawei iBMC(2288H V5,iBMC 6.41)

> 2026-09 真机实测记录(第一台接入的真实硬件)。驱动层的适配已合入代码;
> 本页是兼容矩阵的"现场报告"形态,供其他用户对照。

## 实测环境

| 项 | 值 |
|----|----|
| 机型 | FusionServer / 2288H V5 |
| iBMC 固件 | 6.41(Redfish 1.0.2) |
| 处理器 | 1 × Xeon Silver 4110(8 核) |
| 内存 | 32 GiB |

## 发现与适配

### 1. Session 需要 OEM 字段(已适配,驱动层)

标准 Redfish 会话载荷(`UserName`/`Password`)在 iBMC 上**必拒**(`iBMC.1.0.AuthorizationFailed`),
WebUI 实际发送的载荷带华为 OEM 域:

```json
{"UserName":"...","Password":"...","Oem":{"Huawei":{"Domain":"LocaliBMC"}}}
```

驱动修复:`redfish.createSession` 读取免鉴权的服务根,凭 `Oem.Huawei` 识别厂商,
会话 POST 携带 OEM 域;会话建立后以 `X-Auth-Token` + `Location` 交给 gofish
预认证继续资源遍历。其他厂商走标准载荷。

### 2. 自签名证书(已适配,配置层)

iBMC 默认自签名证书;`MAMMOTH_BMC_TLS_INSECURE=true` 覆盖会话握手与
gofish 资源遍历两处传输。

### 3. ProcessorId/Socket 类型违规(已适配,驱动层宽容解析)

iBMC 的 `ProcessorId.EffectiveFamily` 与 `Socket` 输出**数字**,
Redfish 规范为字符串——严格解析器(gofish)将整个处理器集合判废。
驱动改用宽容解析器:只取 `Model`/`TotalCores`,逐条求和 socket 核数;
`ProcessorSummary` 在该固件上不可信(`Count=1`、`Model="Central Processor"`),仅作回退。

### 4. 盘拓扑不暴露(已知盲区,coverage: partial)

`/Systems/1/Storages`(注意:非标准复数形式,但 gofish 按通告链接可走通)下
RAIDStorage0 控制器可见,但 **Drives 与 Volumes 均为空**——iBMC 6.41 不在
Redfish 上枚举盘拓扑(与 AVAGO 背板管理配置相关)。盘查结果标注
`coverage: partial`、注记 `no drives or volumes reported`,规格级盘查仍完整
(身份/处理器/内存/网卡)。

### 5. 网卡命名(已适配,驱动层)

`EthernetInterface.Name` 为泛化的 "System Ethernet Interface";端口身份在
`Id`(`mainboardLOMPort1`)。驱动改用 `Id` 作为 NIC name,并过滤无 MAC 的
虚拟接口。

## 实测盘查结果(经 mammoth 真实流水线)

```json
{
  "state": "ready",
  "power_state": "on",
  "bmc": {"vendor": "huawei", "model": "2288H V5", "firmware_version": "6.41"},
  "hardware": {
    "serial_number": "2102312…",
    "cpu": {"model": "Intel(R) Xeon(R) Silver 4110 CPU @ 2.10GHz", "cores": 8},
    "memory_bytes": 34359738368,
    "nics": [
      {"name": "mainboardLOMPort1", "mac": "50:1D:93:…"},
      {"name": "mainboardLOMPort2", "mac": "50:1D:93:…"}
    ],
    "coverage": "partial",
    "coverage_notes": ["no drives or volumes reported"]
  }
}
```

### 6. 虚拟介质:Redfish 未通告,绑定 KVM 控制台(实测)

`/Managers/1/VirtualMedia` 下有 CD 与 USBStick 两个资源(`Inserted: False`,
`Image: None`),但 **Actions 完全为空**——`#VirtualMedia.InsertMedia` 未通告。
即 iBMC 6.41 上虚拟介质**只能经 KVM 远程控制台**(WebUI → 远程虚拟控制台 →
虚拟介质,挂本地文件或网络 URL)挂载,Redfish 通道不可用。

驱动行为:检测到动作未通告即返回 `BMC_UNSUPPORTED`(显式降级,
不盲目 POST)——符合 docs/07-bmc.md §1 语义。

**对安装流水线的影响**:此机型上引导介质只能走
(a) KVM 控制台手动挂载(非 API 通路)、(b) PXE(待实现)、
(c) iBMC 本地镜像上传(如固件提供)。
Redfish 虚拟介质可用的机型(如 Dell/HPE 多数型号)不受影响。

### 7. 其它已核实形状

- 存储集合链接为 `/Systems/1/Storages`(非标准复数);gofish 按通告链接可走通;
- `/Systems/1/Processors` 集合本身可用,仅单条目(本机 1 颗物理 CPU);
- 网卡 `Id` 为真实端口身份(`mainboardLOMPort1/2`),`SpeedMbps`/`LinkStatus` 不上报。
