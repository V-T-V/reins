# 杀软误报处置指南 / Antivirus False Positives

## 先说结论

远程运维 Agent 的行为特征——**常驻服务 + 命令执行 + 文件写入 + 屏幕捕获 + 输入注入**——与安全引擎的远程管理风险模型天然重叠。**少量引擎报毒是本品类的行业常态**,不是 reins 被"黑"的证据:

- MeshCentral 的 agent 曾被 Windows Defender 标记为木马,维护者的官方答复是让用户加白名单;**持有有效数字签名的版本仍被 Kaspersky 报过**([MeshCentral #7468](https://github.com/Ylianst/MeshCentral/issues/7468)、[#7256](https://github.com/Ylianst/MeshCentral/discussions/7256))。
- TacticalRMM 官方文档[专设杀软页面](https://docs.tacticalrmm.com/av/),原话是"这个领域充满各种灰色地带的误报"。
- RustDesk 官方签名的安装包在 VirusTotal 上同样有引擎报毒。

**判定标准**:2-5 个引擎报毒、主流引擎(Microsoft/Kaspersky/ESET/Avast)不报 → 属品类常态误报;主流引擎大量报毒 → 请核对哈希后到 [GitHub Issues](../../issues) 报告(先排除下载被劫持/篡改的可能)。

## 第一步:核对哈希(每次都要做)

```powershell
Get-FileHash .\reins-agent-windows-amd64.exe -Algorithm SHA256
# 与 Release 页 SHA256SUMS 逐位比对
```

哈希不匹配说明你的下载源被劫持或文件被篡改,与误报是两回事——请从本仓库 Releases 重新下载。

## Windows Defender

安装路径加入排除(管理员 PowerShell):

```powershell
Add-MpPreference -ExclusionPath "C:\Program Files\reins"
Add-MpPreference -ExclusionProcess "reins-agent.exe"
```

> 排除路径会降低该目录的防护等级,请只对 reins 的安装目录使用,并遵守组织的安全策略。

## 提交误报申诉(维护者会在每个 Release 后统一提交,个人用户也可以)

| 厂商 | 入口 |
|---|---|
| Microsoft | <https://www.microsoft.com/en-us/wdsi/filesubmission>(选 Software developer 身份) |
| Kaspersky | <https://opentip.kaspersky.com/> |
| ESET | <https://www.eset.com/int/business/support/faq/submit-sample/> |
| Avast/AVG | <https://www.avast.com/false-positive-file-form.php> |
| VirusTotal 评注 | 在文件页面留言标注 harmless,高信誉评论者权重更大 |
| 各厂商申诉入口合集 | [False-Positive-Center](https://github.com/yaronelh/False-Positive-Center) |

## 浏览器拦截下载(SmartScreen / "不常见下载")

- SmartScreen 对未积累信誉的新二进制会弹"Windows 已保护你的电脑"——点"更多信息 → 仍要运行",或先校验 SHA256 再运行。
- 长期方案是代码签名(见下),签名证书会随版本积累 SmartScreen 信誉。

## 代码签名现状

见仓库 Release 公告——签名证书到位前,请一律以 **SHA256SUMS + 源码构建** 作为信任锚:reins 是纯 Go 无 C 依赖,`go build ./cmd/reins` 与 `go build ./cmd/reins-agent`(或 `make build`)可从源码复现等价二进制,供应链完全可审计。
