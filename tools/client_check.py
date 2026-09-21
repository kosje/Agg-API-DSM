#!/usr/bin/env python3
"""客户端体检：用真实客户端会发的请求序列打一遍端点。

用法：
    python3 tools/client_check.py <接口地址> <API Key>

例：
    python3 tools/client_check.py https://agg.jr.tn:52325/v1 agg_xxxxx

它会依次做客户端实际会做的事，并在每一步给出可读的结论：

  1. GET  /models            —— 客户端填完地址后会先拉模型列表
  2. POST /chat/completions  —— 非流式
  3. POST /chat/completions  —— 流式（多数客户端默认开，最容易卡在这里）

之所以单独写这个脚本而不是让人去点客户端：客户端只会把失败包装成
「模型服务拒绝了测试请求」这类无信息量的话，看不出是哪一步、什么原因。
这个脚本把每一层的原始响应摊开。
"""
import json
import sys
import time
import urllib.error
import urllib.request

OK, BAD, WARN = [], [], []


def note(kind, text, detail=""):
    mark = {"ok": "OK", "bad": "FAIL", "warn": "WARN"}[kind]
    line = "  [%s] %s" % (mark, text)
    if detail:
        line += "\n         -> " + detail.replace("\n", "\n            ")
    print(line)
    {"ok": OK, "bad": BAD, "warn": WARN}[kind].append(text)


def request(url, method="GET", body=None, key=None, stream=False, timeout=60):
    """发一次请求，返回 (状态码, 响应头, 原始文本)。不抛异常。"""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Authorization", "Bearer " + key)
    if stream:
        req.add_header("Accept", "text/event-stream")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode("utf-8", "replace")
    except Exception as e:
        return 0, {}, "%s: %s" % (type(e).__name__, e)


def short_err(raw):
    """把响应体压成一行，便于贴到聊天里。"""
    try:
        d = json.loads(raw)
        if isinstance(d, dict) and "error" in d:
            e = d["error"]
            if isinstance(e, dict):
                return "%s（type=%s）" % (e.get("message", ""), e.get("type", ""))
            return str(e)
        return raw[:200]
    except Exception:
        return raw[:200]


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    base = sys.argv[1].rstrip("/")
    key = sys.argv[2]

    print("=" * 64)
    print("客户端体检：%s" % base)
    print("=" * 64)

    # ---- 0. 版本与健康 ----
    print("\n0) 服务版本与上游状态")
    root = base[:-3] if base.endswith("/v1") else base
    st, _, raw = request(root + "/healthz")
    version = "?"
    if st == 200:
        try:
            d = json.loads(raw)
            version = d.get("version", "?")
            note("ok", "服务可达，版本 %s" % version)
            for name, v in (d.get("upstreams") or {}).items():
                if v.get("ready"):
                    note("ok", "上游 %s：%s" % (name, v.get("detail")))
                else:
                    note("warn", "上游 %s 不可用：%s" % (name, v.get("detail")))
        except Exception:
            note("warn", "/healthz 返回非 JSON", raw[:120])
    else:
        note("warn", "取不到 /healthz（HTTP %s）" % st, short_err(raw))

    # 流式是 0.3.0 才有的。低于这个版本时客户端默认开流式必然失败 ——
    # 这是最常见的「配好了却用不了」。
    if version not in ("?", "0.3.0") and version < "0.3.0":
        note("bad", "版本 %s 不支持流式（0.3.0 起才有）" % version,
             "多数客户端默认开流式，会直接报「模型服务拒绝了测试请求」。"
             "请先升级套件。")

    # ---- 1. 模型列表 ----
    print("\n1) GET /v1/models")
    st, _, raw = request(base + "/models", key=key)
    if st != 200:
        note("bad", "拉模型列表失败（HTTP %s）" % st, short_err(raw))
        if st == 401:
            note("warn", "鉴权被拒 —— 检查 API Key 是否复制完整（含 agg_ 前缀）")
        return summary()
    try:
        data = json.loads(raw).get("data", [])
    except Exception:
        note("bad", "模型列表不是合法 JSON", raw[:200])
        return summary()
    ids = [m.get("id") for m in data]
    note("ok", "拿到 %d 个模型" % len(ids), ", ".join(ids[:12]))
    if not ids:
        note("bad", "模型列表是空的 —— 上游账号可能一个都没配好")
        return summary()

    model = ids[0]
    # 优先挑一个对话模型：图片模型对纯文本测试不友好
    for pref in ("copilot-auto", "agnes-auto"):
        if pref in ids:
            model = pref
            break
    else:
        for i in ids:
            if "image" not in i.lower():
                model = i
                break
    print("     用 %s 做后续测试" % model)

    # ---- 2. 非流式 ----
    print("\n2) POST /v1/chat/completions（非流式）")
    st, _, raw = request(base + "/chat/completions", "POST", {
        "model": model,
        "messages": [{"role": "user", "content": "Say OK in one word."}],
    }, key=key, timeout=120)
    if st == 200:
        try:
            d = json.loads(raw)
            txt = (d.get("choices") or [{}])[0].get("message", {}).get("content", "")
            note("ok", "非流式可用", "回复：%s" % (txt or "(空)")[:80])
        except Exception:
            note("warn", "200 但响应体解析异常", raw[:200])
    else:
        note("bad", "非流式失败（HTTP %s）" % st, short_err(raw))

    # ---- 3. 流式 ----
    print("\n3) POST /v1/chat/completions（流式，客户端默认就是这个）")
    t0 = time.time()
    st, hdr, raw = request(base + "/chat/completions", "POST", {
        "model": model,
        "stream": True,
        "messages": [{"role": "user", "content": "数到三。"}],
    }, key=key, stream=True, timeout=120)
    elapsed = time.time() - t0

    if st != 200:
        note("bad", "流式失败（HTTP %s，%.1fs）" % (st, elapsed), short_err(raw))
        if st == 501:
            note("warn", "501 表示服务端还不支持流式 —— 升级到 0.3.0 即可")
    else:
        ctype = hdr.get("Content-Type", "")
        if "text/event-stream" not in ctype:
            note("bad", "Content-Type 不是 SSE：%s" % ctype,
                 "中间的反代可能改了响应类型，客户端会解析失败")
        else:
            note("ok", "Content-Type 是 SSE")
        if "data: [DONE]" not in raw:
            note("bad", "没有收到 [DONE] 结束标记",
                 "客户端会一直等收尾，表现成「卡住」")
        else:
            note("ok", "收到 [DONE] 结束标记")
        text = ""
        for line in raw.splitlines():
            line = line.strip()
            if not line.startswith("data:"):
                continue
            p = line[5:].strip()
            if p == "[DONE]":
                continue
            try:
                o = json.loads(p)
            except Exception:
                continue
            for c in o.get("choices", []):
                text += (c.get("delta") or {}).get("content") or ""
        if text:
            note("ok", "流式可用（%.1fs）" % elapsed, "收到：%s" % text[:80])
        else:
            note("bad", "流是空的：收到了帧但没有正文",
                 "前 300 字：%s" % raw[:300])

    return summary()


def summary():
    print("\n" + "=" * 64)
    print("通过 %d 项，失败 %d 项，警告 %d 项" % (len(OK), len(BAD), len(WARN)))
    if BAD:
        print("\n需要处理：")
        for b in BAD:
            print("  - " + b)
    if WARN:
        print("\n建议关注：")
        for w in WARN:
            print("  - " + w)
    if not BAD and not WARN:
        print("\n客户端可以正常使用了。")
    return 1 if BAD else 0


if __name__ == "__main__":
    sys.exit(main())
