"""mock 上游：模拟 Agnes 的 OpenAI 兼容接口。

用于端到端验证网关的完整请求链路，不碰真实上游、零配额消耗。
记录收到的请求头与请求体，便于断言网关是否正确转发了鉴权与参数。
"""
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

RECEIVED = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n)
        RECEIVED.append({
            "path": self.path,
            "auth": self.headers.get("Authorization", ""),
            "ua": self.headers.get("User-Agent", ""),
            "ct": self.headers.get("Content-Type", ""),
            "body": json.loads(body.decode("utf-8")),
        })

        model = RECEIVED[-1]["body"].get("model", "unknown")
        resp = {
            "id": "chatcmpl-mock",
            "object": "chat.completion",
            "model": model,
            "choices": [{
                "index": 0,
                "finish_reason": "stop",
                "message": {"role": "assistant", "content": "mock 回复：" + model},
            }],
            "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10},
        }
        data = json.dumps(resp).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/__received":
            data = json.dumps(RECEIVED).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_response(404)
        self.end_headers()


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18080
    srv = HTTPServer(("127.0.0.1", port), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    print("mock upstream on %d" % port, flush=True)
    threading.Event().wait()
