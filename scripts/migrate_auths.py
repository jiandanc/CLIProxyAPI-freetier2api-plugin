#!/usr/bin/env python3
"""把旧凭证文件迁移到 FreeTier 2API 的命名与字段约定。

做三件事（每个文件）：
  1. type 改为 freetier（所有供应商共用一个 provider key）；
  2. 新增 vendor 字段（供应商归属的最强证据，workbuddycn 等）；
  3. 文件名改为 <vendor>-<uid>.json。

默认只做**预演**（打印将要发生的改动，不碰文件）。确认无误后加 --apply 执行。

**执行时只新增文件，绝不改动原文件**：旧插件（workbuddy2api / qoder2api）
仍要读原来的凭证，删改它们会让那些插件立刻失效。因此本脚本把迁移结果写成
新文件，原文件原样保留。两套文件并存时，旧插件读旧文件、本插件读新文件，
互不干扰。

用法：
  python3 scripts/migrate_auths.py <auths-dir>            # 预演
  python3 scripts/migrate_auths.py <auths-dir> --apply    # 执行
"""

import argparse
import json
import os
import re
import sys

# 统一的 provider key（与 core.ProviderKey 一致）。
PROVIDER_KEY = "freetier"

# 旧文件名前缀 -> 新供应商 ID 的映射。
# 旧文件名里没有区域信息（workbuddy-<uid>.json），因此区域必须从文件内容读。
OLD_PREFIX = "workbuddy"

# 区域 -> 供应商 ID。
VENDOR_BY_REGION = {
    "cn": "workbuddycn",
    "global": "workbuddyglobal",
}

# Qoder 的旧文件名前缀与供应商映射（同样按 region 区分）。
QODER_PREFIX = "qoder"
QODER_VENDOR_BY_REGION = {
    "cn": "qodercn",
    "global": "qoderglobal",
}


def region_of(payload: dict) -> str:
    """从凭证内容推断区域（嵌套形优先，其次扁平形）。"""
    nested = payload.get("auth")
    if isinstance(nested, dict):
        realm = (nested.get("realm") or "").strip().lower()
        if realm:
            return realm
    realm = (payload.get("realm") or payload.get("region") or "").strip().lower()
    return realm


def target_vendor(payload: dict, filename: str) -> str | None:
    """推断目标供应商 ID；推断不出返回 None。"""
    existing = (payload.get("vendor") or "").strip().lower()
    if existing:
        return existing

    region = region_of(payload)
    name = filename.lower()
    if name.startswith(OLD_PREFIX):
        # WorkBuddy 的凭证可能没有 realm（手写/旧格式），按历史数据兜底 cn。
        return VENDOR_BY_REGION.get(region or "cn")
    if name.startswith(QODER_PREFIX):
        # Qoder 国际版是主站，无区域声明时兜底 global。
        return QODER_VENDOR_BY_REGION.get(region or "global")
    return None


def identifier_of(payload: dict, filename: str, vendor: str) -> str:
    """取出用于新文件名的标识（uid / email / 原文件名里的中段）。"""
    nested = payload.get("account")
    if isinstance(nested, dict):
        uid = (nested.get("uid") or "").strip()
        if uid:
            return uid
    for key in ("uid", "email", "qoder_account_id", "label"):
        value = (payload.get(key) or "").strip()
        if value:
            return value
    # 回退：从旧文件名里抠出中段（<prefix>-<中段>.json）。
    stem = re.sub(r"\.json$", "", filename, flags=re.IGNORECASE)
    if "-" in stem:
        return stem.split("-", 1)[1]
    return "account"


def sanitize(value: str) -> str:
    """文件名安全化（与 core.SanitizeFileComponent 同规则）。"""
    out = []
    for ch in value.strip():
        if ch.isascii() and (ch.isalnum() or ch in "._-@"):
            out.append(ch)
        else:
            out.append("-")
        if len(out) >= 64:
            break
    return "".join(out).strip("-.@") or "account"


def plan_for(path: str) -> dict | None:
    """为一个文件算出迁移方案；不需要迁移时返回 None。"""
    filename = os.path.basename(path)
    try:
        with open(path, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except (OSError, json.JSONDecodeError) as err:
        return {"path": path, "error": str(err)}

    vendor = target_vendor(payload, filename)
    if vendor is None:
        return None

    identifier = sanitize(identifier_of(payload, filename, vendor))
    new_name = f"{vendor}-{identifier}.json"
    payload["type"] = PROVIDER_KEY
    payload["vendor"] = vendor
    return {
        "path": path,
        "filename": filename,
        "new_name": new_name,
        "vendor": vendor,
        "payload": payload,
        "rename": new_name != filename,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("auths_dir", help="凭证目录（CPA 的 auth-dir）")
    parser.add_argument("--apply", action="store_true", help="真正执行（默认只预演）")
    args = parser.parse_args()

    if not os.path.isdir(args.auths_dir):
        print(f"目录不存在：{args.auths_dir}", file=sys.stderr)
        return 1

    plans = []
    for name in sorted(os.listdir(args.auths_dir)):
        if not name.lower().endswith(".json"):
            continue
        plan = plan_for(os.path.join(args.auths_dir, name))
        if plan is not None:
            plans.append(plan)

    if not plans:
        print("没有需要迁移的凭证。")
        return 0

    mode = "执行" if args.apply else "预演"
    print(f"=== {mode}：共 {len(plans)} 个文件 ===\n")
    for plan in plans:
        if "error" in plan:
            print(f"  [跳过] {os.path.basename(plan['path'])}：{plan['error']}")
            continue
        arrow = " -> " + plan["new_name"] if plan["rename"] else "（文件名不变）"
        print(f"  {plan['filename']}{arrow}")
        print(f"      vendor={plan['vendor']}  type={PROVIDER_KEY}")

    if not args.apply:
        print("\n这是预演，未改动任何文件。确认后加 --apply 执行。")
        return 0

    print()
    for plan in plans:
        if "error" in plan:
            continue
        path = plan["path"]
        directory = os.path.dirname(path)
        target = os.path.join(directory, plan["new_name"])

        # 只写新文件，原文件保持不动——旧插件还在读它。
        if os.path.exists(target) and os.path.abspath(target) != os.path.abspath(path):
            print(f"  [跳过] {plan['new_name']} 已存在")
            continue
        with open(target, "w", encoding="utf-8") as handle:
            json.dump(plan["payload"], handle, ensure_ascii=False, indent=2)
        os.chmod(target, 0o600)
        print(f"  新增 {plan['new_name']}（原文件 {plan['filename']} 保持不变）")

    print("\n迁移完成：已新增上述文件，原文件未改动。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
