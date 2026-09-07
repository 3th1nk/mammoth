# 06 · 安装流水线

从 Install Spec 到一台就绪的机器,流水线分五个 stage:

```
verify_layout → prepare_media → boot → install → verify_ready
```

## 1. verify_layout(布局校验)

- 校验 spec 中所有 `select` 能在目标机器 `hardware` 上解析出唯一盘;
  多义时结合 `root_device_hints` 消歧,仍多义则任务失败(`SELECT_AMBIGUOUS`);
- `keep: partitions` 声明与最新 layout 快照比对:盘存在、分区号存在;
- 该阶段在提交侧(schema 校验)与执行侧(双重)各做一次——提交侧给用户即时反馈,
  执行侧防提交后机器状态变化。

## 2. prepare_media(介质准备,builder 面执行)

### 2.1 介质策略:发行版原盘 + 通用引导介质

Mammoth **不重打包发行版镜像**。引导采用"双介质"组合:

```
介质 A:发行版原盘 ISO(只读引用,常驻介质仓库,永不修改)
介质 B:通用引导介质(按架构两份常驻;体积 KB~MB 级)
        作用:注入安装器启动参数,指向本任务的应答文件拉取地址
        inst.ks=https://<mammoth>/render/<task-token>/ks.cfg
```

- `<task-token>` 使介质与机器完全解耦:介质可缓存、可并发、可重试;
- 应答文件由服务端按 `spec × machine` 实时渲染,渲染结果快照持久化(调试与审计可离线复盘);
- BMC 需支持同时挂载两个虚拟介质(主流 BMC 均支持);仅支持单介质的 BMC 退化为
  PXE 通道(PXE 预留,roadmap)。兼容矩阵标注各厂商的介质能力。

> 设计原则:**介质与机器解耦**。不为单台机器定制介质——单机制定介质的成本
> (重打包分钟级 + 大文件传输分钟级)在批量场景线性放大,且使介质缓存失效。

### 2.2 应答文件渲染

渲染是纯函数:`render(spec, machine) → answers files`。

- 输入输出全部持久化(spec 快照 + 渲染产物),失败可离线复盘;
- 渲染层不做任何外部调用(不查快照、不连 BMC),保证可测试性与确定性;
- 模板按发行版方言组织,由发行版驱动提供(见 §5)。

## 3. boot(引导)

1. BMC 设置一次性引导(`boot_device once`)+ 挂载双介质;
2. 断电→上电(或 reset),进入安装环境;
3. 等待策略:带内探活轮询(安装器激活网络后可达),超时可配,默认 40 分钟;
4. 引导成功后**立即弹出介质 B**(部分安装器二次重启会再次进入引导介质)。

## 4. install(安装执行)

安装环境内,%pre / early-command 钩子执行 Mammoth 注入的校验-生成脚本:

```
1. 读取实际分区表(lsblk/blkid)
2. 与任务绑定的快照基线比对:
   - preserve 声明的盘/分区是否存在、边界是否一致
   不一致 → 回报 LAYOUT_DRIFT + 终止安装(安装器以显式错误码退出)
3. 一致 → 按意图动态生成最终分区动作:
   wipe 盘 → 清表重建
   keep: disk → 不触碰
   preserve 分区 → 不格式化,按原 uuid 挂载
   其余 → 重建
4. 写出最终应答文件片段,交由安装器执行
```

**这是"保留数据分区"的安全兜底:任何预采集都可能过期,执行现场校验使
"按过期快照装错盘"在机制上不可能发生。**

安装完成后的核验:

- post_install 脚本阶段(scripts.stage=post_install)执行用户注册类脚本;
- 带内探活确认新系统可达(RT 一致性:声明的 IP 即可达的 IP);
- 弹出全部虚拟介质,恢复引导顺序。

## 5. 发行版驱动(OS Driver)

新增发行版 = 新增一个驱动实现 + 模板,**零编排层改动**:

```go
type OSDriver interface {
    Distro() string                                   // rocky9 / ubuntu22 / windows2022 ...
    SupportedArchs() []Arch
    RenderAnswers(spec InstallSpec, m MachineView) ([]AnswerFile, error)
    BootConfig(spec InstallSpec) BootParams           // 安装器启动参数(inst.ks=...)
    KeepPartitionSupport() SupportLevel               // full | partial | none
}
```

- 驱动注册制:`drivers` 注册表按 `spec.image.distro` 路由;
- `KeepPartitionSupport()` 进入发行版支持矩阵,**不支持分区级保留的发行版在提交时即拒绝
  `keep: partitions`**,而不是装到一半失败。

### 发行版支持矩阵(目标态)

| 发行版 | 安装器 | 应答文件 | 保留分区 | 说明 |
|--------|--------|---------|---------|------|
| RHEL 系(Rocky/Alma) | Anaconda | kickstart | **full**(`%pre` + `--onpart/--noformat`) | 首发目标,机制最完整 |
| Debian/Ubuntu | debian-installer / subiquity | preseed / autoinstall | partial | autoinstall 保留分区需 curtin 定制 |
| Windows | Setup | unattend | full | 应答文件体积大,介质策略需验证 |

## 6. 幂等与重试语义

| stage | 重试行为 |
|-------|---------|
| verify_layout | 纯读,直接重跑 |
| prepare_media | 渲染产物按 task-token 幂等(已存在即复用) |
| boot | 重新设置引导并重启;若机器已在安装中,由 attempt 计数与 deadline 判定是否中断重装 |
| install | 以 `%pre` 校验为安全边界:重装前快照漂移会被拦截 |
| verify_ready | 纯读 |

`ForceRetry`(跳过失败 stage 强行续跑)仅限 `verify_ready`;其余 stage 的失败必须
从该 stage 重跑——不存在"跳过校验"的选项。
