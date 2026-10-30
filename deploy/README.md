# deploy/ — 部署目录说明

每种部署形态一个 compose 文件,可选项以 compose profile 挂载——**不叠文件,
按需启用**。运维操作(备份恢复、token 轮换、PXE/NFS/SMB 的完整清单与排障)
在 [docs/operations.md](../docs/operations.md),这里只做地图与选型。

## 文件地图

| 文件 | 作用 |
|------|------|
| `compose.all-in-one.yml` | **拓扑 1(评估/小规模)**:单引擎 `--mode=all` + PostgreSQL。介质组装工具链(builder 镜像)+ 介质目录 bind mount 已内置 |
| `compose.faceted.yml` | **拓扑 2(分面/扩展)**:1×api + N×runner + PostgreSQL,单机或跨机器展开,见下文[跨机器](#跨机器多机器多节点) |
| `compose.tls.yml` + `Caddyfile` | TLS 终止 overlay(生产):与上面任一拓扑叠加 `-f compose.tls.yml`,引擎保持 HTTP,443 由 Caddy 终止(§4.6) |
| `compose.pxe-proxy.yml` + `pxe-proxy.env.example` | **每装机 VLAN 一个**的 Go 原生网段代理(proxyDHCP + TFTP,host 网络):中心引擎不在该 L2 时的 PXE 投递(§4.5 pxe-proxy 节) |
| `Dockerfile.nfsd` + `nfsd/` | 内核 nfsd 管理 sidecar(管理**宿主机内核**的 nfsd 单例;privileged + host 网络) |
| `Dockerfile.samba` + `samba/` | guest 只读 SMB2 sidecar(Windows wimboot setup 通路的安装源;bridge 网络 + 445 即可) |
| `mammoth.env.example` | 全量环境变量模板(`.env` 放同目录即被 compose 读取) |

两个可选 sidecar 在两个拓扑 compose 里同名同 profile,**默认不启动**:

- `--profile nfs` — 内核 NFS(BMC vMedia 与 ubuntu casper nfsroot 的介质源)。
  也可以不用容器,直接用宿主机 nfs-server(定案形态,清单见 §4.7)。
- `--profile samba` — Windows wimboot setup 通路的 SMB 导出;`agent`/`auto`
  安装器不走 SMB,不需要它(§4.5.2)。

```sh
docker compose -f compose.all-in-one.yml --profile nfs --profile samba up -d
```

## 两种拓扑怎么选

| | 拓扑 1 all-in-one | 拓扑 2 faceted |
|---|---|---|
| 进程 | 一个 `--mode=all` 全包 | api(控制面 + 机器面 HTTP + PXE)+ N×runner(任务执行)+ PG |
| 适用 | 评估、小规模(日并发几十台以内、单一 VLAN 足够) | runner 水平扩展、runner 高可用(reaper + 队列重投递)、多装机 VLAN 就近执行 |
| 代价 | 无 | 共享介质目录 + api 负载均衡 + 每 L2 一个 PXE 应答者(见下) |

注意负载的真实位置:装机耗时的大头是 BMC 等待与安装过程,引擎只是编排者;
吞吐瓶颈在**介质构建 CPU**(pool-store 内容寻址已去重)与**安装源拉取带宽**
(机器↔介质服务之间的流量)——扩引擎节点不解决带宽,扩介质服务才解决。

## 跨机器(多机器多节点)

架构原生支持:PG 是唯一强依赖——`SKIP LOCKED` 表队列(advisory lock 互斥、
visibility timeout + reaper 兜底)保证多 runner 消费不重不悬;api 无状态。
跨机器展开时的接线清单:

1. **每个节点同一份 `.env`**(至少 `MAMMOTH_API_TOKEN`/`MAMMOTH_MASTER_KEY`
   一致——主密钥不同则 BMC 凭证不可互解),连同一个 PostgreSQL。
2. **共享介质目录**:机器面 HTTP(`/netboot/files/`、`/render/`、
   `/netboot/store/`)从**本地 MediaDir** 读盘——所有节点 bind mount 同一台
   内核 nfsd 导出的介质目录(§4.7),`MAMMOTH_MEDIA_BASE_URI` 指向它
   (单旋钮双消费者:BMC 与 casper 必须是同一 NFS 服务端,见 roadmap
   2026-10-27 注)。
3. **api 前面挂 LB**(任意四层/七层轮询即可,机器面都是一次性拉取无会话),
   所有节点 `MAMMOTH_EXTERNAL_URL` 指向 LB VIP。
4. **PXE 每 L2 一个应答者**:目标 L2 上的节点开 `MAMMOTH_PXE_ENABLED`
   (api 面,需 host/macvlan 网络,§4.5);其余 VLAN 用 `compose.pxe-proxy.yml`。
5. `MAMMOTH_BMC_TIMEOUT` 等执行面参数在 runner 上调整;控制面 api 不发起
   BMC 调用。

## 镜像

| 镜像 | 基础 | 何时用 |
|------|------|--------|
| `Dockerfile` → distroless | 静态二进制,无 shell | `--mode=api`(纯文件服务/控制面) |
| `Dockerfile.builder` → alpine + xorriso/wimlib/7z | 有 shell、有工具链 | **`--mode=all` 与 `--mode=runner`**(介质组装在进程内 shell 出这些工具,distroless 上必失败);`--mode=builder` 是预留面,serve 启动即拒绝,拆分落地前不要用 |
