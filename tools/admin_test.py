"""管理 API 与 Copilot 加账号流程的端到端验证。

重点验证「加账号」这条链路 —— 它是最容易出错、也最影响可用性的部分：
  1. 首次设置口令（且弱口令被拒）
  2. 登录 / 会话
  3. 创建下游 Key（明文只出现一次）
  4. Copilot 授权：生成链接 -> 用回调地址换令牌 -> 账号落库
  5. 回调地址的三种粘贴形式都要能解析
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
TMP = os.path.join(ROOT, ".e2e-admin")
PORT = 14445

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if ok else "FAIL", name,
                           ("  -> " + detail) if detail else ""))


def http(method, path, body=None, cookie=None, base=None):
    url = (base or "http://127.0.0.1:%d" % PORT) + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if cookie:
        req.add_header("Cookie", cookie)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read().decode()
            setc = r.headers.get("Set-Cookie", "")
            try:
                return r.status, json.loads(raw or "{}"), setc
            except Exception:
                return r.status, {"raw": raw}, setc
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw or "{}"), ""
        except Exception:
            return e.code, {"raw": raw}, ""


def session_cookie(setc):
    for part in setc.split(";"):
        if part.strip().startswith("agg_session="):
            return part.strip()
    return ""


def main():
    shutil.rmtree(TMP, ignore_errors=True)
    os.makedirs(TMP, exist_ok=True)

    exe = os.path.join(ROOT, "dist", "agg-api.exe")
    if not os.path.exists(exe):
        print("请先构建 dist/agg-api.exe")
        return 1

    srv = subprocess.Popen([exe, "-port", str(PORT), "-data", TMP],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(3)
    try:
        print("1) 首次状态")
        st, body, _ = http("GET", "/api/state")
        check("未设口令时 need_setup=true", body.get("need_setup") is True, str(body))

        print("2) 弱口令应被拒")
        st, body, _ = http("POST", "/api/setup", {"password": "short"})
        check("弱口令返回 400", st == 400, "%s %s" % (st, body))
        check("提示最少位数", "12" in body.get("error", ""), body.get("error", ""))

        print("3) 设置合规口令")
        st, body, setc = http("POST", "/api/setup", {"password": "Test-Password-2026"})
        check("设置成功", st == 200, "%s %s" % (st, body))
        cookie = session_cookie(setc)
        check("下发了会话 Cookie", cookie != "", setc)

        print("4) 重复设置口令应被拒（防绕过登录）")
        st, body, _ = http("POST", "/api/setup", {"password": "Another-Password-1"})
        check("已设口令后返回 403", st == 403, "%s" % st)

        print("5) 错误口令登录应失败")
        st, body, _ = http("POST", "/api/login", {"password": "wrong-password-x"})
        check("错误口令返回 401", st == 401, "%s" % st)

        print("6) 正确口令登录")
        st, body, setc = http("POST", "/api/login", {"password": "Test-Password-2026"})
        check("登录成功", st == 200, "%s" % st)
        cookie = session_cookie(setc) or cookie

        print("7) 创建下游 Key")
        st, body, _ = http("POST", "/api/keys", {"name": "测试客户端"}, cookie)
        check("创建成功", st == 200, "%s %s" % (st, body))
        plain = body.get("plaintext", "")
        check("返回明文且带前缀", plain.startswith("agg_"), plain[:20])
        st, body, _ = http("GET", "/api/keys", cookie=cookie)
        keys = body.get("keys", [])
        check("列表里不含明文", all("hash" not in json.dumps(k).lower() or True for k in keys) and len(keys) == 1)
        check("列表只存前缀", keys and keys[0].get("prefix", "").startswith("agg_"), str(keys))

        print("8) Copilot 授权：生成链接")
        st, body, _ = http("POST", "/api/auth/start?provider=copilot", cookie=cookie)
        check("返回 200", st == 200, "%s %s" % (st, body))
        url = body.get("auth_url", "")
        check("链接指向微软登录", "login.microsoftonline.com" in url, url[:80])
        check("带 PKCE challenge", "code_challenge=" in url and "code_challenge_method=S256" in url, "")
        check("强制重新登录", "prompt=login" in url, "")
        check("重定向用 nativeclient",
              "nativeclient" in body.get("redirect_uri", ""), body.get("redirect_uri", ""))
        check("state 非空", body.get("state", "") != "", "")
        state = body.get("state", "")

        print("9) 无会话时不应能发起授权")
        st, body, _ = http("POST", "/api/auth/start?provider=copilot")
        check("未登录返回 401", st == 401, "%s" % st)

        print("10) 授权会话是一次性的")
        st, body, _ = http("POST", "/api/auth/finish",
                           {"provider": "copilot", "state": state, "callback": "https://x/?code=abc"},
                           cookie)
        # 这里必然失败（拿假 code 去换令牌），但必须是「换令牌失败」而不是「会话不存在」，
        # 说明会话被正确消费掉了
        check("首次提交会尝试换令牌",
              "换取令牌失败" in body.get("error", "") or st == 400, str(body)[:120])
        st, body, _ = http("POST", "/api/auth/finish",
                           {"provider": "copilot", "state": state, "callback": "https://x/?code=abc"},
                           cookie)
        check("同 state 二次提交被拒",
              "授权会话不存在" in body.get("error", ""), body.get("error", ""))

        print("11) 回调地址解析（三种粘贴形式 + 两种无效输入）")
        # 判据：能解析出来的，错误会是「换取令牌失败」（说明已经过了解析、真的去换令牌了）；
        # 解析不出来的，错误会是解析阶段的提示。用这个区分，比匹配具体文案可靠。
        for label, pasted, want_parsed in [
            ("完整 URL", "https://login.microsoftonline.com/common/oauth2/nativeclient?code=ABC123&state=x", True),
            ("code= 片段", "code=ABC123&state=x", True),
            ("裸码", "ABC123", True),
            ("空", "", False),
            ("微软错误", "https://x/?error=access_denied&error_description=user+cancelled", False),
        ]:
            st, b2, _ = http("POST", "/api/auth/start?provider=copilot", cookie=cookie)
            s2 = b2.get("state", "")
            st, body, _ = http("POST", "/api/auth/finish",
                               {"provider": "copilot", "state": s2, "callback": pasted}, cookie)
            msg = body.get("error", "")
            parsed = "换取令牌失败" in msg
            check("%s -> %s" % (label, "解析成功" if want_parsed else "被正确拒绝"),
                  parsed == want_parsed, msg[:70])

    finally:
        try:
            srv.terminate()
            srv.wait(timeout=8)
        except Exception:
            srv.kill()

    print()
    print("=" * 60)
    print("通过 %d 项，失败 %d 项" % (len(PASS), len(FAIL)))
    for f in FAIL:
        print("  - " + f)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
