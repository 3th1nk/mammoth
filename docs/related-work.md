# 相关开源项目对照(related work)与借鉴清单

mammoth 的生态位:**不依赖任何平台的独立裸金属装机引擎,以发行版原生应答
文件方言(kickstart / autoinstall / preseed)表达安装意图**。主流开源项目
均未轻量占据此位:Ironic 绑定 OpenStack,Metal3 绑定 Kubernetes,
Tinkerbell 换用 agent 工作流哲学,Pixiecore 只做引导层。

| 项目 | 本质 | 硬依赖 | 安装逻辑 | 与 mammoth 的关系 |
|------|------|--------|----------|-------------------|
| Ironic | OpenStack BMS 组件 | OpenStack 全栈 | IPA agent + deploy steps | 同域,重平台 |
| Metal3/BMO | K8s BMH 控制器 | K8s + Ironic | IPA(同 Ironic) | 同域,K8s 生态 |
| Tinkerbell | 工作流引擎 | 多组件 | osie hook 镜像 | 同域,agent 哲学 |
| Pixiecore | PXE 引导器 | 无 | 无(引导层) | M6 PXE 的实现参考 |

## 借鉴清单(按 roadmap 对应)

1. **M6 PXE**:Pixiecore(Apache-2.0,Go)的 **proxyDHCP** 模式——不抢占
   现有 DHCP,旁路应答 PXE 客户端;其 "boot from API" 动态引导与 mammoth
   的 task-token URL 同构。
2. **ramdisk 探针**:Tinkerbell osie(内存 OS)与 IPA 的 hardware
   collection——dmidecode/lsblk/ipmitool 输出的结构化 schema 可直接对照;
   osie 还能刷固件/配 RAID,是探针的激进版形态。
3. **擦盘合规**:Ironic cleaning steps(NIST 800-88)可作为 wipe 策略的
   未来选项(superblock 清除之外的合规等级)。
4. **API 错误模型**:Metal3 BareMetalHost 的 ErrorMessage 分类与
   operational status 映射,供任务错误码的产品化呈现参考。
5. **反面参考**:IPA agent 模式要求包源能被 agent 消费(镜像制式限制);
   mammoth 的原生应答文件方言让安装逻辑落在发行版最成熟的路径上——
   这是架构上坚持方言抽象(而非自造 agent)的依据。

## 相关结论

- 三方言 × 发行版的真机矩阵验证(rocky9/ubuntu22/debian12/uniontechos)
  完成后,mammoth 在"无平台依赖 + 原生方言"维度上具备独立生态位;
- 与上述项目共存而非竞争:业务层若已有 OpenStack/K8s,可经其 API 驱动
  mammoth(mammoth 保持 API-first,任何编排器都可调用)。

## BMC 驱动与异构服务器适配(Ironic 沉淀的借鉴方式)

Ironic 的厂商硬件管理沉淀在 Python 库生态(proliantutils=HPE iLO、
python-dracclient=Dell iDRAC、irmc-client=富士通……),**语言不通,无法直接
搬代码**;可借鉴的是它的**适配组织方式**:

1. **现代服务器已 Redfish 统一**:Dell iDRAC9+、HPE iLO5+、Supermicro X11+
   原生 Redfish——mammoth 的 Redfish 驱动天然覆盖主流机型;Ironic 的厂商
   Python 库主要服务老固件/非 Redfish 设备(mammoth 由 IPMI 驱动兜底)。
2. **quirk 沉淀点 = bmccompat 兼容矩阵**:逐厂商真机实录(华为模式)——
   Ironic 驱动代码里的厂商坑(缺失字段、必须走 OEM 端点的操作、固件版本
   分界)正是我们的 compat 条目来源;Ironic 的已知名单可当**测试清单**用。
3. **能力接口模型**:Ironic 的 `supported_*_interfaces` ↔ mammoth 的能力
   接口(VolumeCreator/PhysicalDrives 已有)。可新增两个标准 Redfish 能力:
   - **BiosSetter**(Bios Registry:属性表读/写,厂商无关);
   - **FirmwareInventory**(SoftwareInventory 只读,固件基线核对)。
4. **cleaning steps 模型**:Ironic 的 NIST 800-88 擦盘规范任务化——wipe
   策略(superblock/整盘)之外的未来合规等级选项。
