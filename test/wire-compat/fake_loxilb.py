#!/usr/bin/env python3
"""Fake loxilb REST endpoint for kube-loxilb wire-payload A/B testing.

Records every non-GET request body to a jsonl file and answers with a
configurable result so the client-side error paths can be exercised.

env:
  FAKE_PORT   listen port (default 11111)
  FAKE_REC    record file (default ./record.jsonl)
  FAKE_MODE   normal   - 200 {"result":"Success"}         (default)
              conflict - 409 {"result":"lb rule exists"}  (duplicate rule)
              notfound - 404 {"result":"not-exists"}      (missing rule)
  FAKE_VERSION  plain   - 200, no `product` field (upstream loxilb)  (default)
                missing - 404 (loxilb too old to have /version)
                gateway - 200 with product=loxilb-inference-gateway
"""
import json, os, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("FAKE_PORT", "11111"))
REC = os.environ.get("FAKE_REC", "record.jsonl")
MODE = os.environ.get("FAKE_MODE", "normal")
VMODE = os.environ.get("FAKE_VERSION", "plain")

def record(method, path, body):
    with open(REC, "a") as f:
        f.write(json.dumps({"ts": round(time.time(), 3), "method": method,
                            "path": path, "body": body}, sort_keys=True) + "\n")

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _send(self, code, obj):
        payload = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        p = self.path.split("?")[0].rstrip("/")
        if p.endswith("/version"):
            if VMODE == "missing":
                return self._send(404, {"result": "not found"})
            v = {"version": "0.9.9", "buildInfo": "fake"}
            if VMODE == "gateway":
                v["product"] = "loxilb-inference-gateway"
            return self._send(200, v)
        if p.endswith("/config/loadbalancer/all"):
            return self._send(200, {"lbAttr": []})
        if p.endswith("/config/cistate/all"):
            return self._send(200, {"Attr": []})
        if "/config/loadbalancer/status" in p or p.endswith("/status"):
            return self._send(404, {"result": "not-exists"})
        return self._send(200, {})

    def _read_body(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        try:
            return json.loads(raw) if raw else None
        except Exception:
            return {"__unparsed__": raw.decode("utf-8", "replace")}

    def do_POST(self):
        p = self.path.split("?")[0]
        body = self._read_body()
        record("POST", p, body)
        if "/config/loadbalancer" in p:
            if MODE == "conflict":
                return self._send(409, {"result": "lb rule exists", "message": "conflict"})
            if MODE == "notfound":
                return self._send(404, {"result": "not-exists", "message": "not found"})
        return self._send(200, {"result": "Success"})

    def do_DELETE(self):
        p = self.path.split("?")[0]
        record("DELETE", p, self._read_body())
        return self._send(200, {"result": "Success"})

if __name__ == "__main__":
    print(f"fake-loxilb :{PORT} mode={MODE} version={VMODE} rec={REC}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
