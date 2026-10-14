# Redfish walk 改造真机回归操作清单

> **状态**：待执行。代码面已由单测收口(collection/walk 共 16 个用例,含跨页
> 拼接/自环/降级/nextLink 形态归一),本清单覆盖单测给不了的三个真机证据:
> ①walk 回归(.74 可随即跑,~10 分钟只读);②BIOS 生效纪律对照实测(随
> BiosSetter 真机窗口,roadmap 第 2 项本就挂账);③分页生效验证(.74 给不了,
> 触发条件见 §3)。
>
> 改动背景(2026-10-14,批量交付实战模式沉淀):Redfish 集合分页
> (`Members@odata.nextLink`,部分厂商 75 条/页,DSP0268 集合 oldest→newest
> 序,不跟页则日志恰恰丢最新记录)+ BIOS pending 值生效纪律 + 上传挂死知识。
> 详见 docs/07-bmc.md §6.1 生效纪律、docs/compat/patterns.md P8。

## 1. 只读冒烟(walk 回归确认,192.168.1.74 / iBMC 6.41)

改动触及四处 collection walk(SEL 的 Entries/Managers/LogServices、健康面
Chassis、固件清单 FirmwareInventory、RAID Volumes)。对**不分页固件**新代码
与旧代码等价(无 nextLink 即单页返回),本节验证的是"没改坏"。

```bash
# serve 按 2288H 现场参数(HANDOFF 2026-09-19 记录):
#   MAMMOTH_MEDIA_DIR/WORKDIR/BASE_URI=/data/mammoth/{media,work,…}
#   MAMMOTH_BMC_TLS_INSECURE=true(iBMC 6.41 证书无 IP SAN)
# 机器已注册:mch_d5b1609a0290(iBMC 192.168.1.74,凭证 cred_b12d6ad8425e)
M=mch_d5b1609a0290
H="Authorization: Bearer $MAMMOTH_API_TOKEN"
BASE=http://localhost:8080/api/v1

# 1) 带外盘查(触发 FirmwareInventory / Storage / 网卡全 walk)→ 202 + job
curl -s -H "$H" -X POST $BASE/machines/$M/actions \
  -d '{"type":"discover","probe":"redfish"}'
# 等 job 终态后核对固件落档:
curl -s -H "$H" $BASE/machines/$M | jq '.firmware'

# 2) 四个只读活读端点(全部应 200,不 502/422)
for ep in health sel bios drives; do
  echo "== $ep =="; curl -s -H "$H" $BASE/machines/$M/$ep | head -c 400; echo
done

# 3) SEL 内容 sanity:倒序、不空(机器开机过就该有记录)
curl -s -H "$H" $BASE/machines/$M/sel | jq '.entries | length, .[0]'
```

**通过标准**:

- discover 成功,`machines.firmware` 有固件条目(walk 全链无破);
- health 返回传感器表且 overall 非 error;iBMC 6.41 既往实测传感器可读;
- sel 倒序、条目数与改版前同量级(该固件 Entries 一次全量,应恰好等于
  改版前结果——这是**行为不变**的最直接证据);
- bios/drives 200。

## 2. BIOS pending 生效纪律对照实测(随 BiosSetter 真机窗口)

docs/07-bmc.md §6.1 生效纪律目前是**实战模式知识**(GracefulRestart/OS 内
reboot 不保证触发 BIOS 应用),华为行为未经本机实测。利用 BiosSetter 回归
窗口做一组对照,把纪律变成 compat/huawei.md 的实测证据:

1. 选一个**无害、可复读**的属性(布尔/枚举,避开 boot 设备、安全、RAID
   相关键;属性名以 `GET /machines/{id}/bios` 活表为准);
2. `set_bios_attributes`(confirm:true)→ **soft_reboot** → 复读活表:
   记录属性是回读为新值还是仍为旧值(pending 未应用);
3. `set_bios_attributes` → **power cycle**(硬断电循环)→ 复读活表:
   预期新值落定;
4. 把两行结果回填 compat/huawei.md(§BiosSetter 段),若华为 graceful 即
   生效,在 docs/07-bmc.md §6.1 生效纪律处补"华为例外"注记——纪律文案
   写的是最保守保证,厂商差异照实标注。

**通过标准**:cycle 后属性落定;soft_reboot 分支的行为如实记录(无论哪
种结果都是有价值的 compat 证据)。实测完把测试属性改回原值并确认。

## 3. 分页生效验证(192.168.1.74 无法完成,触发条件制)

iBMC 6.41 的 LogService Entries 一次全量返回,**本机证明不了分页修复
生效**——它只能证明 §1 的回归。真验证需要一台真分页的控制器(批量交付
实战中 75 条/页那类,典型为 DGX/Supermicro 世代 BMC)且 SEL 攒满一页以上。
届时:

- `GET /machines/{id}/sel` 返回条目数 **> 单页容量**(即翻页真的发生,
  可对控制器裸 curl `/redfish/v1/.../Entries` 数 `Members@odata.count`
  或看 nextLink 是否存在);
- 倒序首位是**最新**记录(修复前该形态下返回的是最旧一页——修复的
  直接目标);
- 顺手看该固件 nextLink 的 wire 形态(绝对路径/完整 URL),与
  collection.go nextLinkFor 的三种归一分支对号,记入 compat 矩阵。

## 4. 不需要真机的部分(已收口,列此备查)

- 集合分页/降级/防环/nextLink 形态归一:internal/bmc/redfish
  collection_test.go + health_test.go,16 用例;
- 上传挂死(Expect: 100-continue):Go net/http 不自动发该头,现有通路
  免疫;预防性知识入 patterns.md P8,落地项(固件上传/staged 上传)实现
  时按 P8 自查,届时假 BMC 补"无 100 响应"用例 + 真机首验上传。
