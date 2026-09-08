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

### 6. 虚拟介质:OEM VmmControl 动作,仅支持 NFS/CIFS(实测)

`/Managers/1/VirtualMedia` 下有 CD 与 USBStick 两个资源,但标准
`#VirtualMedia.InsertMedia` 动作**未通告**(Actions 为空)。真正的通路是
华为 OEM 动作:

```
POST /redfish/v1/Managers/1/VirtualMedia/CD/Oem/Huawei/Actions/VirtualMedia.VmmControl
{"Image": "nfs://<server>/<export>/<file>.iso", "VmmControlType": "Connect"}
```

- `Connect` 返回 202 + Redfish Task(轮询至终态;`Exception` 时读 Messages);
- **`http://` URI 被直接拒绝**:`iBMC.1.0.FileTransferProtocolMismatch`
  ("image URI 与动作参数中的传输协议不匹配")——即该固件的 VmmControl
  仅支持 NFS/CIFS 类共享,不支持 HTTP 直链;
- `Disconnect` 为同步弹出(实测:`eject_media` 动作 → 任务 succeeded →
  Redfish `Inserted: False`),契约已补 `eject_media` 动作类型;
- 连接失败回报 `iBMC.1.0.ConnectionFailed`。

驱动修复:`redfish.MountMedia/EjectMedia` 在标准动作未通告时回退到
VmmControl(任务轮询至终态,失败分类为 BMC_PROTOCOL_ERROR 并携带 iBMC
消息)。**部署要求:机器可访问的 NFS 服务导出镜像目录**(也可用 CIFS)。

**✅ 实测挂载成功**:`mount_media` + `nfs://198.51.100.248/data/os_iso/.../Rocky-9.7-x86_64-minimal.iso`
→ VmmControl Connect → 任务 succeeded → Redfish `Inserted: true`、`Image` 指向该 NFS URI、
`MediaTypes: ["CD"]`。注意 URI 路径必须精确到 NFS 导出目录下的真实文件
(HTTP 根目录与 NFS 导出目录是两个不同路径,别混淆)。

**对安装流水线的影响**:此机型上 image.source 与介质地址应使用
`nfs://host/export/xxx.iso` 形态;引导介质注入(kickstart)在该机型上
仍受 KVM 通路限制(见 §6 上述),完整安装需 NFS 介质 + PXE/KVM 组合。

### 7. 一次性引导(实测)

`BootSourceOverrideTarget=Cd + Enabled=Once` 设置成功,重启进入安装器后
**自动回退**(`Target: None / Enabled: Disabled`)——一次性语义在真机正确,
无需驱动侧补偿(07-bmc.md §4 的"非一次性引导补偿"在此机型不需要)。

### 8. 软重启(实测)

`GracefulRestart` 在此固件/电源状态下被拒("Correct the value for the
parameter")——使用 `ForceRestart`(hard_reboot)成功。驱动侧该映射本就
显式,无需改动;操作上软重启失败时用 hard_reboot 替代。

### 9. 其它已核实形状

- 存储集合链接为 `/Systems/1/Storages`(非标准复数);gofish 按通告链接可走通;
- `/Systems/1/Processors` 集合本身可用,仅单条目(本机 1 颗物理 CPU);
- 网卡 `Id` 为真实端口身份(`mainboardLOMPort1/2`),`SpeedMbps`/`LinkStatus` 不上报。
