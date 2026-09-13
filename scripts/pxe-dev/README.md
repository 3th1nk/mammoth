# pxe-dev — 本机 PXE 四跳验证 harness

验证 mammoth 的 proxyDHCP + TFTP + iPXE 链路(qemu guest → mammoth)。
**需要 Linux + root**(bridge/tap/KVM),照 `probe_dev` 先例:不进 CI,
仅开发机手动跑。macOS 上只能跑到"协议单测"那层(仓库 `go test
./internal/netboot/...` 已覆盖 DHCP/TFTP/脚本渲染)。

> 为什么不能用 `qemu -netdev user`:slirp 的内建 DHCP 运行在 qemu 进程内,
> 宿主上的 proxyDHCP 根本收不到 guest 的广播,也无法与之共存——必须真网桥
> + 真 DHCP。

## 拓扑

```
qemu guest (tap-pxe → br-pxe)
   ├─ dnsmasq:站点 DHCP,只分地址(绝不配 dhcp-boot——否则验证的不是我们)
   └─ mammoth:proxyDHCP(67/4011)+ TFTP(69)+ 机器面 HTTP(8080)
        br-pxe 网关 IP = 192.168.77.1(即 MAMMOTH_PXE_NEXT_SERVER)
```

## 步骤

```sh
# 1. 网络侧(root)
sudo scripts/pxe-dev/net.sh up

# 2. mammoth(root 或 setcap cap_net_bind_service=+ep;67/69/4011 是特权端口)
sudo env MAMMOTH_PXE_ENABLED=true \
    MAMMOTH_PXE_NEXT_SERVER=192.168.77.1 \
    MAMMOTH_EXTERNAL_URL=http://192.168.77.1:8080 \
    MAMMOTH_DATABASE_URL='postgres://…' \
    MAMMOTH_API_TOKEN=devtoken MAMMOTH_MASTER_KEY=$(openssl rand -base64 32) \
    ./bin/mammoth serve --mode=all

# 3. 提交一次 pxe 策略安装(机器 MAC 即 qemu 里的 52:54:00:12:34:56;
#    先 discover 让库存有 NIC MAC,再:)
curl -H "$AUTH" -d '{"type":"install","targets":{"machine_ids":["mch_…"]},
  "spec":{"image":{"distro":"rocky9","source":"https://…/Rocky.iso"},
          "boot":{"strategy":"pxe"}}}' localhost:8080/api/v1/jobs

# 4. 起 guest(另一个 root 终端;UEFI 用 FIRMWARE=uefi)
sudo FIRMWARE=bios scripts/pxe-dev/qemu-pxe.sh
```

## 四跳验证链(逐跳看日志)

1. **DHCP OFFER**:mammoth 日志/抓包确认 proxyDHCP 应答了 DISCOVER
   (yiaddr=0,opt67=undionly.kpxe 或 ipxe-amd64.efi);
2. **TFTP**:NBP 被取走(未知文件保持静默是设计行为);
3. **iPXE 脚本**:`GET /netboot/script?mac=…`(事件 `task.netboot_script_served`);
4. **安装器**:kernel/initrd 经 `/netboot/files/<token>/` 拉取,`inst.repo=nfs:`
   拿包,`inst.ks=` 回 mammoth,%post 完成回调 → 任务 succeeded。

ramdisk 探针 PXE:`probe=ramdisk` + `boot=pxe` 提交 discover job,验证
`machine.probe_reported` 事件与 ramdisk 快照落库(alpine overlay 第二段
cpio 追加 + `modloop=<url>` 在此一并验证——真机回归前先过这里)。

## 清理

```sh
sudo scripts/pxe-dev/net.sh down
rm -f /tmp/pxe-dev-disk.qcow2
```
