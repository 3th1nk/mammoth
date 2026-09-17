# 跨厂商装机模式与共性坑(从厂商实测中沉淀)

来源是各厂商 compat 文档(huawei.md 等)里反复出现的结构性问题。这里记录
**模式与对策**,厂商文档记录**实测证据**;新厂商/新机型联调前对照本清单,
新方言/新载体实现时逐条自查。每条注明:问题本质 → 对策 → 已落地的方言/
代码位置。

## P1 控制器卷名 ≠ 安装器内核名(最高频)

- **本质**:Redfish/BMC 呈现的逻辑卷名(`LogicalDriveN`)在安装器内核里
  不存在(实际为 `/dev/sdX` + SCSI serial);且 mammoth 的带内快照可能
  同样只带控制器视角——**内核名只存在于机器上**,渲染侧无从得知。
- **对策(三层,缺一即复发)**:
  1. serial 优先:绑定卷刷新出 `ID_SCSI_SERIAL` 时按 serial 匹配
     (curtin)/`--serial`( Anaconda);注意 `ID_SERIAL_SHORT` 与
     `ID_SCSI_SERIAL` 是两个值,以安装器实测探测语义为准;
  2. 渲染侧不可解时**留占位 path + 机上解析**:安装器早期钩子按
     size±1%(lsblk / /sys/block)定位并改写应答文件/存储配置;
  3. 快照交叉校验:渲染时对内核名目标做 ±1% size 漂移检查,漂移即
     报错要求重探(快照新鲜度防线)。
- **已落地**:kickstart %pre / debian partman/early_command(resolve-disk.sh)/
  autoinstall early-commands(resolve-disk.sh,改写 /autoinstall.yaml)。
- **教训**:机制问题不许留"联调手段"尾巴(huawei.md 曾以"盘查记录改名
  sda"跑通,换载体后第三次复发);一个方言修过不代表其他方言免疫。

## P2 引导早期网络不稳:DHCP 时序竞态

- **本质**:initramfs 的网络配置发生在 udev 网卡改名前后、NFS 跳板之前,
  按设备名配置会打空(改名竞态);DHCP 广播应答在部分固件/驱动组合下
  引导期收不到(引导后同一机器立即正常)。
- **对策**:①内核参数按 MAC 钉设备(BOOTIF=01-<mac> / ifname=m0:<mac>);
  ②有 DHCP 池时 arm 时预留地址(粘性),引导参数直接写静态 `ip=`,
  引导期零 DHCP;③casper/klibc 的 nfsmount 不认 `proto=`,TCP 要写
  `nfsopts=tcp,v3`,且现代 nfsd 多已无 v3/UDP。
- **已落地**:autoinstall(BOOTIF+静态 ip=)、preseed(netcfg 走内核参数,
  URL seed 在 netcfg 之后加载——netboot 载体的静态网配必须走内核参数)、
  kickstart(ifname=)。

## P3 PXE 参数行被引导器截断

- **本质**:GRUB 把 `;` 当命令分隔符——`ds=nocloud-net;s=URL` 之后的
  全部内核参数被静默丢弃(内核只收到前几个参数,报错毫无提示)。
- **对策**:渲染进 grub.cfg 的内核参数一律转义 `;` 为 `\;`(sanitizeArgs);
  回归测试须断言"参数整行完整"而非仅首参数存在。
- **已落地**:internal/netboot RenderGRUB/sanitizeArgs。

## P4 安装器"缺文件就死等"与探测文件

- **本质**:TFTP 服务对不存在的文件静默丢弃时,shim 等引导程序的可选
  文件探测(revocations_*、shim_certificate_*)会无限重试;安装器组件
  拉取(anna/apt)对镜像索引的任何不一致(校验和、by-hash 货栈、签名)
  表现为死循环或笼统的 mirror 错误。
- **对策**:TFTP 对 miss 必须回 ERROR(内部服务通用要求);离线池须
  四件套自洽:udeb 补齐、Release 四段校验重写、by-hash 货栈回填、
  armor 签名;trixie+ 的 apt 在 target 内验签且用 sqv——签名钥匙必须
  经 debootstrap 进 target(单文件 deb + 索引 stanza,stanza 需空行闭合,
  debootstrap 比 apt 挑剔)。
- **已落地**:internal/netboot/tftp.go、internal/builder/netboot_pool.go、
  poolkey.go。

## P5 磁盘治理:介质构建的满盘与孤儿

- **本质**:每任务介质树(PXE ~1-2.5G)在任务中断/服务重启窗口积累;
  满盘表现为构建工具(xorriso 等)半途天书报错。
- **对策**:①构建前置水位闸(不足即拒,错误信息点名清理路径);
  ②任务终态(成功/失败/取消)统一释放 + reaper 孤儿清扫按 TTL 兜底,
  释放入口按 flow 无关化(install/discover 同构)。
- **已落地**:internal/provision/strategy_pxe.go(requireDiskHeadroom)、
  reaper.sweepNetboot、runner 取消路径。

## P6 安装"看似卡住"的常态与可观测性

- **本质**:subiquity 装机画面数小时不动是常态(虚拟光驱 ~0.4MB/s);
  安装器日志是 ramfs,重启即逝;安装器环境自带 sshd,易与装好的系统
  混淆(假阳性)。
- **对策**:verify_ready 对安装器会话继续轮询(ENV 节探测 /run/anaconda
  等);完成回调和六阶段事件是进度判断的唯一事实源;安装器 syslog 拷入
  target 备查(d-i);中期:netboot syslog sink + `syslog=` 内核参数
  (related-work §2)。
- **已落地**:inbandssh ENV 探测、preseed post-install 拷日志;
  syslog sink 为近期行动项。

## 使用方式

- 新机型联调:先对照 P1-P4 的"证据形态"识别问题类别,再去厂商文档找
  实测细节;
- 新方言/新载体实现:P1 三层与 P2 ①③ 是自查清单,缺任一层视为未完成;
- 本文只写模式,不写厂商参数;实测证据、quirk 细节、固件版本分界见各
  厂商 compat 文档(huawei.md)。
