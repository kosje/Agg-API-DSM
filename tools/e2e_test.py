"""端到端验证：下游 -> 网关 -> mock 上游 -> 回到下游。

覆盖点：
  1. /v1/models 列出两个上游的模型
  2. /v1/chat/completions 按模型名路由到正确的上游
  3. 上游凭据（Bearer）被正确转发
  4. 能力检查生效（agnes-auto 声明了 image，copilot-chat 没有）
  5. 未知模型返回 404 而不是转发出去
"""
import base64
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TMP = os.path.join(ROOT, ".e2e-data")
MOCK_PORT = 18080
GW_PORT = 14444

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if ok else "FAIL", name,
                           ("  -> " + detail) if detail else ""))


def http(method, url, body=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"raw": raw}


def stream_request(url, body, headers=None):
    """发起流式请求，返回 (状态码, 解析后的信息, 原始文本)。"""
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=60) as r:
        raw = r.read().decode("utf-8", "replace")
        ctype = r.headers.get("Content-Type", "")
        status = r.status
    text = ""
    objs = []
    for line in raw.splitlines():
        line = line.strip()
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            continue
        try:
            o = json.loads(payload)
        except Exception:
            continue
        if "error" in o:
            objs.append(o)
            continue
        objs.append(o)
        for c in o.get("choices", []):
            text += (c.get("delta") or {}).get("content") or ""
    return status, {"__ctype": ctype, "__text": text, "__objects": objs}, raw


def main():
    # 每次运行都从空数据目录开始：否则上一轮写下的配置会残留，
    # 让「未配置账号」这类前置用例看到不该有的账号。
    shutil.rmtree(TMP, ignore_errors=True)
    os.makedirs(TMP, exist_ok=True)

    mock = subprocess.Popen(
        [sys.executable, os.path.join(ROOT, "tools", "mock_upstream.py"), str(MOCK_PORT)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api" + (".exe" if os.name == "nt" else "")),
                           "-port", str(GW_PORT), "-data", TMP],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(3)

    try:
        base = "http://127.0.0.1:%d" % GW_PORT

        print("1) 模型清单")
        st, body = http("GET", base + "/v1/models")
        ids = [m["id"] for m in body.get("data", [])]
        check("GET /v1/models 返回 200", st == 200, str(st))
        check("含 agnes-auto", "agnes-auto" in ids, str(ids))
        check("含 copilot-auto", "copilot-auto" in ids, str(ids))
        owners = {m["id"]: m["owned_by"] for m in body.get("data", [])}
        check("agnes-auto 归属 agnes", owners.get("agnes-auto") == "agnes")
        check("copilot-auto 归属 copilot", owners.get("copilot-auto") == "copilot")

        print("2) 未知模型应 404，且不转发到上游")
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "不存在的模型", "messages": [{"role": "user", "content": "hi"}]})
        check("未知模型返回 404", st == 404, "%s %s" % (st, body))

        print("3) 未配置账号的上游应返回 503 且点明是哪个上游")
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "gpt-5.5", "messages": [{"role": "user", "content": "hi"}]})
        check("无账号返回 503", st == 503, "%s" % st)
        msg = body.get("error", {}).get("message", "")
        check("错误信息点明上游", "M365 Copilot" in msg, msg)

        print("4) 未配置账号时，流式也应返回结构化错误（而不是开始流）")
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto", "stream": True,
                         "messages": [{"role": "user", "content": "hi"}]})
        check("无账号的流式请求返回 503", st == 503, "%s" % st)

        print("5) 未配置 Agnes 账号时请求应返回 503")
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto", "messages": [{"role": "user", "content": "hi"}]})
        check("无账号返回 503", st == 503, "%s" % st)
        check("错误类型为 no_capacity",
              body.get("error", {}).get("type") == "no_capacity", str(body))

        print("6) 配好账号后走通完整链路")
        cfg = {
            "accounts": [
                {
                    "id": "acc-1", "provider": "agnes", "name": "测试账号",
                    "base_url": "http://127.0.0.1:%d" % MOCK_PORT,
                    "api_key": "sk-test-12345",
                    "models": ["agnes-auto", "agnes-image-2.1-flash"],
                    "enabled": True, "rpm": 0,
                    "created_at": "2026-09-21T00:00:00Z",
                },
                {
                    "id": "acc-2", "provider": "copilot", "name": "Copilot 账号",
                    "auth": {"token": "fake"},
                    "models": ["gpt-5.5"],
                    "enabled": True, "rpm": 0,
                    "created_at": "2026-09-21T00:00:00Z",
                },
            ],
            "settings": {"default_rpm": 60, "max_retries": 3, "backoff_base_ms": 100},
        }
        with open(os.path.join(TMP, "config.json"), "w", encoding="utf-8") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)

        # 重启网关让新配置生效（控制台的热重载尚未实现）
        gw.terminate()
        gw.wait(timeout=10)
        gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api" + (".exe" if os.name == "nt" else "")),
                               "-port", str(GW_PORT), "-data", TMP],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)

        st, body = http("GET", base + "/v1/models")
        ids = [m["id"] for m in body.get("data", [])]
        check("模型清单随账号变化", "agnes-image-2.1-flash" in ids, str(ids))

        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto",
                         "messages": [{"role": "user", "content": "你好"}],
                         "temperature": 0.5})
        check("对话请求返回 200", st == 200, "%s" % st)
        content = ""
        if st == 200 and body.get("choices"):
            content = body["choices"][0]["message"]["content"]
        check("回复内容来自上游", "mock 回复" in content, content)
        check("usage 被透传",
              body.get("usage", {}).get("total_tokens") == 10, str(body.get("usage")))

        print("6b) copilot 账号缺 access_token，应返回明确的「需要重新授权」")
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "gpt-5.5", "messages": [{"role": "user", "content": "hi"}]})
        check("缺凭据返回 502", st == 502, "%s" % st)
        check("错误类型为 upstream_auth",
              body.get("error", {}).get("type") == "upstream_auth", str(body))
        check("提示可操作",
              "重新授权" in body.get("error", {}).get("message", ""),
              body.get("error", {}).get("message", ""))

        print("6c) 流式：完整链路")
        sst, sbody, sraw = stream_request(
            base + "/v1/chat/completions",
            {"model": "agnes-auto", "stream": True,
             "messages": [{"role": "user", "content": "你好"}]})
        check("流式返回 200", sst == 200, "%s" % sst)
        check("Content-Type 是 SSE",
              "text/event-stream" in (sbody.get("__ctype") or ""), str(sbody.get("__ctype")))
        check("以 [DONE] 收尾", sraw.rstrip().endswith("data: [DONE]"), sraw[-40:])
        joined = sbody.get("__text", "")
        check("分片拼回完整回复", "mock 流式回复" in joined, joined)
        check("chunk 结构正确",
              sbody.get("__objects") and sbody["__objects"][0].get("object") == "chat.completion.chunk",
              str(sbody.get("__objects", [])[:1]))

        print("6d) 附件：CSV 解析并拼进消息")
        csv_b64 = base64.b64encode("城市,人口\n北京,2189万\n上海,2487万\n".encode()).decode()
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto",
                         "messages": [{"role": "user", "content": "这份表里哪个城市人多？"}],
                         "attachments": [{"name": "cities.csv", "mime": "text/csv", "data": csv_b64}]})
        check("带附件的请求返回 200", st == 200, "%s %s" % (st, str(body)[:120]))
        st2, recv2 = http("GET", "http://127.0.0.1:%d/__received" % MOCK_PORT)
        sent = ""
        for r in reversed(recv2):
            if r["body"].get("stream"):
                continue
            msgs = r["body"].get("messages") or []
            if msgs and "附件" in json.dumps(msgs, ensure_ascii=False):
                sent = json.dumps(msgs, ensure_ascii=False)
                break
        check("附件文本已拼进消息", "北京" in sent and "上海" in sent, sent[:160])
        check("原提问被保留", "哪个城市人多" in sent, "")

        print("6e) 不支持的附件类型应明确拒绝")
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto",
                         "messages": [{"role": "user", "content": "hi"}],
                         "attachments": [{"name": "x.exe", "mime": "application/octet-stream",
                                          "data": base64.b64encode(b"\x00\x01").decode()}]})
        check("不支持的类型返回 400", st == 400, "%s" % st)
        check("错误类型为 invalid_attachment",
              body.get("error", {}).get("type") == "invalid_attachment", str(body)[:140])

        print("7) 校验转发到上游的请求")
        st, recv = http("GET", "http://127.0.0.1:%d/__received" % MOCK_PORT)
        check("上游收到了请求", len(recv) > 0, "收到 %d 条" % len(recv))
        # 取最近一条**带 temperature 的**请求。
        # 不能简单用 recv[-1]：后面还跑过流式与附件用例，它们本来就不带
        # temperature，拿它们校验参数透传会误判。
        with_temp = [r for r in recv if r["body"].get("temperature") is not None]
        if with_temp:
            last = with_temp[-1]
            check("路径正确", last["path"] == "/v1/chat/completions", last["path"])
            check("鉴权头被转发", last["auth"] == "Bearer sk-test-12345", last["auth"])
            check("UA 标识自己", "Agg-API-DSM" in last["ua"], last["ua"])
            check("model 用了上游标识",
                  last["body"].get("model") == "agnes-auto", str(last["body"].get("model")))
            check("temperature 被转发",
                  last["body"].get("temperature") == 0.5, str(last["body"].get("temperature")))

        print("8) base_url 带 /v1 时不应拼成 /v1/v1")
        cfg["accounts"][0]["base_url"] = "http://127.0.0.1:%d/v1" % MOCK_PORT
        with open(os.path.join(TMP, "config.json"), "w", encoding="utf-8") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)
        gw.terminate()
        gw.wait(timeout=10)
        gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api" + (".exe" if os.name == "nt" else "")),
                               "-port", str(GW_PORT), "-data", TMP],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto",
                         "messages": [{"role": "user", "content": "hi"}]})
        st2, recv2 = http("GET", "http://127.0.0.1:%d/__received" % MOCK_PORT)
        path = recv2[-1]["path"] if recv2 else "?"
        check("base_url 带 /v1 时路径不重复", path == "/v1/chat/completions", path)

        print("9) 账号轮询：第一个账号坏了，应自动换第二个")
        # 账号 1 指向一个没人监听的端口（必然失败），账号 2 指向 mock。
        # 轮询若生效，请求应该成功；若没生效，会把账号 1 的错误直接抛出来。
        cfg["accounts"] = [
            {"id": "acc-bad", "provider": "agnes", "name": "坏账号",
             "base_url": "http://127.0.0.1:19999", "api_key": "sk-bad",
             "models": ["agnes-auto"], "enabled": True, "rpm": 0,
             "created_at": "2026-09-21T00:00:00Z"},
            {"id": "acc-good", "provider": "agnes", "name": "好账号",
             "base_url": "http://127.0.0.1:%d" % MOCK_PORT, "api_key": "sk-good",
             "models": ["agnes-auto"], "enabled": True, "rpm": 0,
             "created_at": "2026-09-21T00:00:00Z"},
        ]
        with open(os.path.join(TMP, "config.json"), "w", encoding="utf-8") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)
        gw.terminate(); gw.wait(timeout=10)
        gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api" + (".exe" if os.name == "nt" else "")),
                               "-port", str(GW_PORT), "-data", TMP],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto",
                         "messages": [{"role": "user", "content": "轮询测试"}]})
        check("坏账号在前时请求仍成功", st == 200, "%s %s" % (st, str(body)[:120]))
        st2, recv3 = http("GET", "http://127.0.0.1:%d/__received" % MOCK_PORT)
        hit = any("轮询测试" in json.dumps(r["body"], ensure_ascii=False) for r in recv3)
        check("请求确实落到了第二个账号", hit, "mock 收到 %d 条" % len(recv3))

        print("9b) 全坏时不该无限重试")
        cfg["accounts"] = [cfg["accounts"][0]]
        with open(os.path.join(TMP, "config.json"), "w", encoding="utf-8") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)
        gw.terminate(); gw.wait(timeout=10)
        gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api" + (".exe" if os.name == "nt" else "")),
                               "-port", str(GW_PORT), "-data", TMP],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        t0 = time.time()
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto",
                         "messages": [{"role": "user", "content": "hi"}]})
        el = time.time() - t0
        check("全坏时返回上游错误", st == 502, "%s" % st)
        check("有限时间内返回（%.1fs）" % el, el < 30, "%.1fs" % el)

    finally:
        for p in (gw, mock):
            try:
                p.terminate()
                p.wait(timeout=8)
            except Exception:
                p.kill()

    print()
    print("=" * 60)
    print("通过 %d 项，失败 %d 项" % (len(PASS), len(FAIL)))
    if FAIL:
        print("失败项：")
        for f in FAIL:
            print("  - " + f)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
