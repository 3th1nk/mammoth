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
