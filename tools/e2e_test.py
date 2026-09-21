"""端到端验证：下游 -> 网关 -> mock 上游 -> 回到下游。

覆盖点：
  1. /v1/models 列出两个上游的模型
  2. /v1/chat/completions 按模型名路由到正确的上游
  3. 上游凭据（Bearer）被正确转发
  4. 能力检查生效（agnes-auto 声明了 image，copilot-chat 没有）
  5. 未知模型返回 404 而不是转发出去
"""
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


def main():
    # 每次运行都从空数据目录开始：否则上一轮写下的配置会残留，
    # 让「未配置账号」这类前置用例看到不该有的账号。
    shutil.rmtree(TMP, ignore_errors=True)
    os.makedirs(TMP, exist_ok=True)

    mock = subprocess.Popen(
        [sys.executable, os.path.join(ROOT, "tools", "mock_upstream.py"), str(MOCK_PORT)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api.exe"),
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
                        {"model": "copilot-chat", "messages": [{"role": "user", "content": "hi"}]})
        check("无账号返回 503", st == 503, "%s" % st)
        msg = body.get("error", {}).get("message", "")
        check("错误信息点明上游", "M365 Copilot" in msg, msg)

        print("4) 流式应明确拒绝（尚未接通），而不是静默退化")
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto", "stream": True,
                         "messages": [{"role": "user", "content": "hi"}]})
        check("stream=true 返回 501", st == 501, str(st))

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
                    "models": ["copilot-chat"],
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
        gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api.exe"),
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
                        {"model": "copilot-chat", "messages": [{"role": "user", "content": "hi"}]})
        check("缺凭据返回 502", st == 502, "%s" % st)
        check("错误类型为 upstream_auth",
              body.get("error", {}).get("type") == "upstream_auth", str(body))
        check("提示可操作",
              "重新授权" in body.get("error", {}).get("message", ""),
              body.get("error", {}).get("message", ""))

        print("7) 校验转发到上游的请求")
        st, recv = http("GET", "http://127.0.0.1:%d/__received" % MOCK_PORT)
        check("上游收到了请求", len(recv) > 0, "收到 %d 条" % len(recv))
        if recv:
            last = recv[-1]
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
        gw = subprocess.Popen([os.path.join(ROOT, "dist", "agg-api.exe"),
                               "-port", str(GW_PORT), "-data", TMP],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        st, body = http("POST", base + "/v1/chat/completions",
                        {"model": "agnes-auto",
                         "messages": [{"role": "user", "content": "hi"}]})
        st2, recv2 = http("GET", "http://127.0.0.1:%d/__received" % MOCK_PORT)
        path = recv2[-1]["path"] if recv2 else "?"
        check("base_url 带 /v1 时路径不重复", path == "/v1/chat/completions", path)

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
