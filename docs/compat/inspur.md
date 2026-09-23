# 厂商实录:Inspur 浪潮 BMC(文档推演,待真机复核)

> 与 [h3c.md](h3c.md) 同形态:全部内容摘自官方文档两册——
> **《浪潮英信服务器 Redfish 用户手册》(文档版本 1.1,2022-05-24)**
> (驱动协议主源)+ **《浪潮英信服务器 BMC 用户手册》(文档版本 V2.9,
> 2022-10-27)**(Web GUI / IPMI / Smashclp CLI 三视角,交叉验证与运维
> 注记)。与既有驱动的对号推演,**尚未经任何真机验证**。矩阵 YAML 在
> 真机复核前保持为空——本页回答"接上浪潮时该预期什么、先验什么"。
>
> 与 H3C 的关键区别:**本厂商预期零驱动改动**——虚拟介质走标准动作,
> 会话/引导/电源全部命中现有防御;价值在两处接入预警与文档基线。

## 底座判定:AMI(文档级样例证据)

三条来自手册示例数据的证据(均为出厂默认形态,弱于 HDM 的响应头实证):

1. SSL 证书样例(§6.53):`Subject: CN=www.ami.com, OU=Service
   Processors, O=American Megatrends Incorporated`——AMI 出厂自签证书;
2. 系统 HostName 样例(§7.2):`AMIB4055D8F2C84`——AMI 前缀 + MAC;
3. Manager Model(§6.2):`ast2500`——ASPEED BMC 芯片,AMI 常见组合;
4. **第二册真机输出样例**(§4.6.2):`diagnose cat cpuinfo` →
   `Hardware : AST2500EVB`(ARM926EJ-S,part 0xb76;meminfo 共
   400MB BMC 内存)——**带内实测输出**,底座判定从"出厂样例推断"
   升级为"实测级";
5. 第二册 DNS 配置页 HostName 样例(§3.11.1.2):`AMIB4055D8F2C2`
   ——AMI 前缀 + MAC 生成规则的第二例(与第 2 条同款)。

**这是 AMI MegaRAC 行的第二个文档级样本,且第二册把证据推到实测级**。
与 H3C 对照,同一底座内
虚拟介质路径已经分化:浪潮通告标准动作,H3C 走 OEM 命名空间——
"底座共性"不能替代按厂商对号,mammoth 的"通告优先 → OEM 回退"分层
设计恰好覆盖两种形态。

## 文档基线

| 项 | 值 |
|----|----|
| 适用 | 浪潮英信 **M6 系 21 款**(NF/i/SA/SN;两路/四路/AI/多节点——多节点电源/风扇经 CMC 另册)。样例 NF5280M6 / A320 |
| 规范基线 | DSP0266 1.8.0 / DSP0268 2019.2(RedfishVersion 1.8.0) |
| 鉴权 | Basic / X-Auth-Token 会话,**标准载荷** |
| 会话超时 | SessionTimeOut 可创建时指定:300-1800s,60 的整数倍;第二册:Web 默认 30min(=1800s 上限同源) |
| BMC 型号(样例) | ast2500(第二册带内实测 `AST2500EVB`) |
| 介质协议 | **仅 NFS**(虚拟介质与固件升级两处一致;第二册:Web GUI 另有 CIFS,Web-only) |
| 管理面(第二册) | Web GUI(HTTPS 443,HTTP 默认禁用)/ IPMI 2.0(KCS、LANPLUS、IPMB)/ Smashclp over SSH / SNMP v1-v3 |
| 并发容量(第二册) | Web 20 会话;本地用户 16(IPMI 2.0 模型);IPMI 会话 36(样例);KVM 4 |
| 默认凭据(第二册) | admin/admin;U-Boot `inspur@u600t`;SNMP 团体字 `inspur@0531`(读=写);调试串口 sysadmin/superuser。手册明示**首次登录提示修改默认口令** |

## IPMI 兜底路径(第二册全文档化,mammoth 免适配)

第二册把带外/带内 IPMI 底座交代得完整——与底座顺口溜"IPMI 强
Redfish 弱"相反,浪潮是 **IPMI/Redfish 双完整**的现代代际。README
底座行的"IPMI 兜底路径可用"在浪潮上有了文档细节:

- **通道**(表 2-1):`0x01` Primary LAN(专用口,千兆)/ `0x08`
  Secondary LAN(共享口 NCSI,**百兆上限**)/ `0x0F` KCS 带内。
  网络默认"绑定自适应"模式(专口/共享口同 MAC,专口在位则共享口
  禁用)——走专用口无感;若被迫走共享口,别按千兆估带宽;
- **密码套件**(表 2-2):1/2/3/6/7/8/11/12 之外 **15/16/17
  (RAKP-HMAC-SHA256 族)在列**——`ipmitool -C 17` 直接可用,安全
  加固现场不必降到 SHA1/MD5;
- **SOL 激活不支持**:SOL 配置参数 Set/Get 均支持,但 **SOL
  Activating(Transport 0x20)= NO**——串口重定向这条路在浪潮关死,
  装机过程观察只能走 KVM(与 §9"无凭证深链"叠加:KVM 价值更高而
  入口更受限);
- **OEM 诊断通道**:一键收集日志进度 `ipmitool raw 0x3C 0x44`
  (NetFn 0x3C):`0xfc` 完成 / `0xfe` 收集中 / `0xfd` 开始 /
  `0xfa` 未开始 / `0xfb` 压缩失败 / `0xf1` 删旧失败,附进度字节与
  ASCII 文件名,全程 1~2 分钟——诊断采集的现成 OEM 入口;
- **带内 KCS**:OS 可达时 `ipmitool` 走 `0x0F` 自访——runner 与目标
  机同机场景的兜底通道,免网络面;
- 自检代码(Get Self Test Results)为 IPMI 标准表(0x55 =
  SFT_CODE_OK),附录照抄规范,无浪潮私货。

## 与既有防御的对号(逐条预期)

### 1. 会话:标准载荷命中,但 token 来源要核(零改动 + 预检点)

标准 `{"UserName","Password","SessionTimeOut"}` POST /Sessions,201 返回
——驱动的 `createSession` 按服务根 Oem 识别厂商,非华为走标准载荷,
预期零改动。**手册的关键 quirk(§3.5)**:token 出现在
**响应体** `Oem.Public["X-Auth-Token"]`;而手册未展示
`Location` 响应头形态。gofish 预认证链路按 `X-Auth-Token + Location`
交接——若现场响应头不带 Location,需要驱动层从响应体 Oem 补取
(候选适配,不是已确认缺陷)。**真机预检第一位**。第二册印证:Web
会话超时默认 30 分钟(=1800s);并发 Web 20/IPMI 36(样例)——驱动的
单会话缓存远低于任一档。

### 2. If-Match 全覆盖(预期零改动)

手册所有 PATCH(连 PATCH /redfish/v1 根服务都)要求 If-Match,取法
"GET 相应 URL 从响应头读 ETag"。驱动 Boot PATCH 携带资源 ETag、
BIOS 写活读 ETag 的既有实现预期直接命中。

### 3. 电源:ResetType 全集 + AllowableValues 通告(预期零改动)

`#ComputerSystem.Reset` 通告完整集合:`On / ForceOff /
GracefulShutdown / GracefulRestart / ForceRestart / Nmi / ForceOn /
PushPowerButton / ForcePowerCycle`(§7.2 实证带
`ResetType@Redfish.AllowableValues`)。与 iBMC 相反,浪潮是全集厂商,
驱动的"预读 + 拒绝即回退"逻辑回退分支永不触发。注意另有
**Chassis.Reset**(§5.4)为子集(On/ForceOff/ForceRestart/Nmi/
PowerCycle)——驱动走系统级,不触此路径。第二册 Web 电源控制页动作
全集(开机/强制关机/强制关机再开机=延时10s/强制系统重启/触发 NMI/
软关机)与 Redfish 通告同源——触发 NMI 在浪潮是 Web 一等动作,
PowerAction 扩展时可参考。

### 4. 一次性引导:三参数同发,Target 无 BiosSetup(预期零改动)

PATCH Boot 要求 Target 与 Enabled 成对同发(文档注释"需和...一起使用"),
Mode 随行——驱动单请求整发三属性命中。目标集 `None / Pxe / Cd / Hdd`
(**无 BiosSetup**,比 HDM 少);Enabled 集 `Once / Continuous /
Disabled`。一次性语义是否如 iBMC 一样自动回退待真机复核。第二册
注记:Web 的 BIOS 启动选项有第五个目标"启动时进入 BIOS 设置界面"
(Redfish 未通告)——目标集差异属**接口子集**而非能力缺失,进
BIOS Setup 走 Web/KVM;时效两档(仅下次/未来所有)与 Once/Continuous
对应。

### 5. 虚拟介质:标准 InsertMedia 通告命中(零改动,两处预警)

§6.27 实证 CD 槽位资源**通告标准动作**:

```
"Actions": {
  "#VirtualMedia.InsertMedia": {"target": ".../Actions/VirtualMedia.InsertMedia"},
  "#VirtualMedia.EjectMedia":  {"target": ".../Actions/VirtualMedia.EjectMedia"}
}
```

驱动 `MountMedia` 的"标准动作优先 → OEM 回退"链路在通告即命中,
**不会落入 `/Oem/Huawei/`、`/Oem/Public/` 候选试探**——这是
vmm.go 候选序设计对非 OEM 厂商的正确行为(通告 target 排最前)。
挂载为**同步 200**(`Oem.Public.Status: 0`),无任务轮询。槽位
CD + USBStick,`ConnectedVia: URI`。

两处接入预警:

- **TransferProtocolType 必带**:手册要求显式 `"NFS"`
  (目前仅支持该值)。README 预警表"缺 TransferProtocolType 被拒"
  行对浪潮直接适用;mammoth 当前 `nfs://` scheme 自描述协议,是否
  触发拒绝待真机——**被拒时先试剥 scheme + 显式 TransferProtocolType**;
- **Image 形态**:手册示例为裸主机路径
  (`"100.2.52.82/home/nfs/VBoxGuestAdditions_6.1.12.iso"`,
  无 scheme)。若固件对 scheme 严格,同上剥 scheme 兜底。

部署要求同华为:机器可达的 NFS 导出镜像目录——mammoth 内置 go-nfs
只读导出天然适配。第二册交叉:Web GUI"一般设置"的共享类型为
nfs/**cifs 双选**,但 CIFS 是 **Web-only** 能力,API 侧未文档化——
勿据 Web 能力推断 API 支持;镜像格式 ISO9660 / UDF(v1.02-v2.60),
`*.iso` / `*.nrg`。

### 6. 存储:标准单数路径 + 建卷同步方言(盘查零改动,建卷 roadmap)

- 集合路径 `/Systems/{id}/Storage`(**标准单数**)——与 iBMC/HDM 的
  非标 `Storages` 复数相反。gofish 按通告链接遍历,零改动;
  同一 AMI 底座内存储路径形态已随 OEM 分化,按通告走是唯一正解;
- 物理盘(`/Storage/{id}/Drives`)SerialNumber/Model/Protocol 齐全,
  `Oem.Public` 额外给 **VolumeName(所属卷名)/ Slot / RaidName /
  FWState**——比华为的 DriveID 更直接:物理盘↔逻辑卷关联现成,
  `PhysicalDrives` 枚举与 serial 映射的候选数据源;
- Chassis 背板盘(§5.8)字段可空(SerialNumber/Model 为 null),
  宽容解析已有;
- **建卷是第三种方言**:POST Volumes → **200 同步**
  `{"cc": 0, "Status": "OK"}`(非 202+任务,也非 HDM 的 error 信封),
  且载荷按 RAID 卡品牌分三种(PMC 卡 / Marvell 卡 / 通用 Redfish
  载荷 RaidLevel+SpanDepth+Drives)。本轮不实现,真机有 RAID 需求
  时先验品牌分支;删卷 DELETE 标准;
- OEM 动作 `SetBootDrive / GetBootDrive`(ctrlIndex+ldTarget)设引导盘
  ——对应华为的 BootEnable 属性,roadmap;
- 第二册表 3-19 在册 RAID/SAS 卡为 BRCM/Inspur/MCHP(Microchip)
  三家——第一册"建卷载荷按 PMC/Marvell/通用三分支"的硬件背景,
  真机建卷先认卡再选载荷分支。

### 7. BIOS 写与安全(预期低改动命中)

- 属性写:活读 `/Bios` 属性表 → PATCH `/Bios/Settings` + If-Match,
  重启生效——与 iBMC/HDM 同形;
- **撤销 pending**:OEM 动作 `Oem/Public/Settings.Revoke`
  (§7.9,**DELETE 方法**)——与华为 `Oem.Huawei #Settings.Revoke`
  对应,若未来实现 revoke 需 OEM 域 + 动词双适配;
- `Bios.ChangePassword`(PasswordName=AdministratorPassword)与
  `Bios.ResetBios` 标准,文档级可用。

### 8. SEL 与传感器(预警一条)

SEL 走 `/Managers/1/LogServices/SEL/Entries` + `$skip/$top` 分页,
`Members@odata.nextLink` 指引——若驱动读取假设全量单页,大日志
机器上会截断(预警,待核驱动读取方式)。Oem.Public 下另有
ThresholdSensors/DiscreteSensors 非标资源。Manager.Reset 仅
ForceRestart(§6.33)。第二册印证:SEL 上限 **3639 条**(真机
Smashclp 输出 `# of Alloc Units: 3639`)+ 满员循环覆盖——大日志
机器上分页必然触发;另审计日志 200K 上限、浪潮独有 IDL 日志
(8 字节事件码:级别/偏移/部件序号/部件类型,每条带处理建议)。

### 9. KVM(保持 UNSUPPORTED)

`/Managers/1/KvmService` 资源(会话上限 4,Shared/Private),
NetworkProtocol 通告 KVMIP 端口(样例 7578)。**手册未给任何
token 深链/SSO 形态**——与华为结论同源:无凭证深链可合成,
ConsoleURL 保持 `BMC_UNSUPPORTED`。OEM 截屏动作(触发/下载)仅
运维诊断用,roadmap。第二册注记:KVM 三客户端——H5Viewer
(HTML5,websockify 走 **443**,免 Java)/JViewer(Java JNLP,
OpenJDK 1.8+,**部分机型不支持**)/VNC(5900/5901,非安全口默认
关);**官方明示不支持经代理连接 BMC**("nginx 能开 Web 但 Java
控制台打不开 JViewer")——架构注记:若未来经反代暴露 BMC,H5
可通,VNC/JViewer 必须直连端口。

### 10. BMC 固件升级(运维注记 + roadmap)

SimpleUpdate 标准 target + OEM 参数(`FlashItem: BMC/BIOS/CPLD`、
`PreserveConf`、`BiosFlash: Flash1/Flash2/Both`),传输协议
**SCP/SFTP/NFS(无 HTTP)**,FirmwareInventory 含
ActiveBMC/BackupBMC/Bios(Updateable/Version)——
FirmwareInventoryProvider 候选。刷新状态是 OEM 数字
(0 完成/1 失败/2 中/3 无任务/4 未开始)。

**运维注记(手册 §1 明示)**:更新 BMC 固件后,"访问硬件(如
PCIe 设备等)及 BIOS 相关接口信息需重启一次系统,请等待 BIOS POST
完成拿到资产及配置信息"——BMC 升级后立即跑盘查/BIOS 读会拿到
过期数据;流水线若引入 BMC 升级,升后需 power cycle 再采集。

第二册运维细节:双 64M Flash 双镜像 + **刷新失败自动回滚** +
防错刷(跨厂商/型号/固件类型拒刷);**BIOS/CPLD 更新触发条件
POWEROFF——电源开着任务不触发,文档建议先 `ipmitool power off`
后自动触发**;保留配置勾选项含 Redfish 下发的 BIOS 配置;异步更新
=刷完不重启、下次重启切新镜像并同步备镜像;Smashclp
`mc --set dualimgconf <0-5>` 可显式选启动镜像(Higher/Lower/
IMAGE-1/2/Newest/Not-newest)——升级失败回切通道。流水线若引入
升级:BIOS/CPLD 先 power off,升后 power cycle 再采集(两册交叉
印证)。

## Roadmap 候选(文档可见、mammoth 暂无对应能力)

- **BMC 配置导入/导出**(ExportConfFile/ImportConfFile,multipart)、
  BIOS 选项配置导入导出——装机前基线化;
- **事件订阅**(标准 EventService/Subscriptions,5 类事件,
  HttpHeaders 可携带 X-Auth-Token)——告警接入候选;
- **网卡/RAID 配置导出**(NIC/RAID.ExportConfiguration)、
  VLAN CRUD、主机网口 Configure(OEM 动作);
- 固件升级链(见 §10)、KVM 截屏诊断(宕机截屏 IERR 触发/宕机录像
  .dat→.avi)、Syslog/SNMP/SMTP 告警;
- **一键收集日志**(IPMI OEM `raw 0x3C 0x44` 状态机,1~2 分钟;
  onekeylog 打包含 SEL/审计/IDL/SMBIOS/RAID 卡日志)——装后诊断
  取证候选。

## 待真机预检清单(30 分钟,对齐 README 预检流程)

0. **凭据前置**:出厂默认 admin/admin,手册明示"首次登录提示修改
   默认口令"——若现场开启强制首改,先走一次改密,避免把首改拦截
   误判成会话故障(§1);
1. **会话(第一位)**:标准载荷 POST /Sessions → 记录**响应头**
   `Location`/`X-Auth-Token` 是否存在,对照响应体
   `Oem.Public["X-Auth-Token"]`(决定 gofish 预认证是否需驱动兼容);
2. **盘查**:跑一次 `discover` → 核 `/Systems/1/Storage`(单数)
   路径、物理盘 SerialNumber 与 Oem.Public.VolumeName/Slot;
3. **虚拟介质**:`mount_media`(`nfs://` 形态直发)→ 若 InsertMedia
   被拒,试剥 scheme + 显式 `TransferProtocolType: NFS`;盯
   `Inserted: true` 回读(同步无任务);
4. **引导**:`set_boot_device(Cd, once)` → 回读三值正确?安装器
   进引导后是否自动回退;
5. **电源**:`soft_reboot`/`hard_reboot` 各一发(全集下回退不触发);
6. **ETag**:PATCH Boot 后重 GET,核对 ETag 变化;
7. **SEL**:翻页 nextLink 行为;
8. (有 RAID 卡)建卷:手工 POST 一次,取证 200+`cc:0` 形态与
   品牌载荷分支;
9. (可选)IPMI 兜底:`ipmitool -I lanplus -C 17 sdr elist` 通带外
   (顺带记录 cipher 兼容);OS 在位时再试带内 KCS。

预检全绿后按 README 矩阵格式补 `inspur.yaml`(known_defects 需固件
区间),本页"待真机复核"逐条回填。
