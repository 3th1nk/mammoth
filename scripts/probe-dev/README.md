# scripts/probe-dev — ramdisk 探针(discover)本地开发环

在 qemu 里快速迭代 ramdisk 探针(docs/05-inventory.md §4,discover 任务的
载 体):改探针代码 → `go test -tags probe_dev` 重打 ISO → qemu 引导 →
本地 catch server 收报告,分钟级闭环,不占用真机。

## 组成

| 文件 | 作用 |
|------|------|
| `make-testdisk.py` | 生成 8MB 原始磁盘镜像(单 MBR 分区,LBA 2048 起),给探针的 /sys 扫描路径喂一块"sda+sda1" |
| `qemu-boot.sh` | 引导探针 ISO(bios/uefi 两种固件形态),等待探针回报后自动关机;slirp 网络下探针经 10.0.2.2 访问宿主 |
| `report-server.py` | 探针回报的 catch server:任意路径的 POST 都收,正文落盘,常驻到 Ctrl-C |

## 用法

```sh
python3 make-testdisk.py /tmp/probe-testdisk.raw          # 1. 造测试盘
python3 report-server.py --port 8765 --out /tmp/report.json &   # 2. 起 catch server
go test -tags probe_dev ./...                              #    (重打探针 ISO,报告 URL 指向 10.0.2.2:8765)
./qemu-boot.sh uefi <probe.iso> /tmp/report.json           # 3. 引导并收报告
```

## 注意

- 探针 ISO 内的报告 URL 是构建期固定的(qemu slirp 网段 10.0.2.2),换端口需同步重打 ISO;
- UEFI 形态用于覆盖 OVMF 引导结构(历史问题见 docs/compat/huawei.md ramdisk 回顾);
- 真机排障走 runbook(各机型 runbook),本目录仅服务本地开发环。
