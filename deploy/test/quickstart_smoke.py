"""Isolated kind CI only: real login, saved settings, repeat-install data safety."""
import base64
import http.cookiejar
import json
import subprocess
import time
import urllib.request

ORIGIN = "http://127.0.0.1:18080"
cookies = http.cookiejar.CookieJar()
browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies))


def kubectl(*args):
    return subprocess.check_output(["kubectl", "-n", "aegisops", *args], text=True)


def request(path, data=None, method=None):
    headers = {"Content-Type": "application/json", "Origin": ORIGIN}
    csrf = next((cookie.value for cookie in cookies if cookie.name == "aegisops_dev_csrf"), None)
    if csrf:
        headers["X-CSRF-Token"] = csrf
    body = None if data is None else json.dumps(data).encode()
    req = urllib.request.Request(ORIGIN + path, body, headers, method=method)
    with browser.open(req, timeout=15) as response:
        return json.load(response)["data"]


def wait_forward():
    for attempt in range(30):
        try:
            with urllib.request.urlopen(ORIGIN + "/readyz", timeout=2):
                return
        except OSError:
            time.sleep(1)
    raise RuntimeError("port forward did not become ready")


secret = json.loads(kubectl("get", "secret", "aegisops-credentials", "-o", "json"))
password = base64.b64decode(secret["data"]["password"]).decode()
before = kubectl("get", "pods", "-l", "component=server", "-o", "jsonpath={.items[0].metadata.uid}")
forward = subprocess.Popen(["kubectl", "-n", "aegisops", "port-forward", "svc/aegisops-server", "18080:8080"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    wait_forward()
    request("/api/v1/auth/login", {"username": "admin", "password": password})
    assert any(cookie.name == "aegisops_dev_session" for cookie in cookies), "HTTP login did not set a session cookie"
    request("/api/v1/settings/model", {"max_tokens": 2048}, "PATCH")
    subprocess.run(["bash", "deploy/install.sh"], check=True)
    after = kubectl("get", "pods", "-l", "component=server", "-o", "jsonpath={.items[0].metadata.uid}")
    # Ephemeral experience must not rebuild; persistent experience may restart.
    values = json.loads(kubectl("get", "configmap", "aegisops-installation", "-o", "json"))["data"]
    if values["persistence"] == "false":
        assert before == after, "repeat install rebuilt ephemeral data"
    # Reconnect in case a persistent rollout replaced the forwarding Pod.
    if before != after:
        forward.terminate()
        forward.wait(timeout=10)
        forward = subprocess.Popen(["kubectl", "-n", "aegisops", "port-forward", "svc/aegisops-server", "18080:8080"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait_forward()
        request("/api/v1/auth/login", {"username": "admin", "password": password})
    view = request("/api/v1/settings/model")
    assert any(item["key"] == "max_tokens" and item.get("value") == 2048 for item in view["items"]), "saved model configuration lost"
    print("quickstart login, settings save and repeat-install persistence passed")
finally:
    forward.terminate()
    forward.wait(timeout=10)
