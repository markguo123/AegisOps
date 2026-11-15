#!/bin/bash
set -u
NS=aegisops
H="https://aegisops.10.62.107.200.nip.io"
JAR=/tmp/fix.jar
rm -f $JAR
PASSW=$(kubectl -n $NS get secret aegisops-credentials -o jsonpath='{.data.password}' | base64 -d)
QE="你能看下test-complex这个ns下面的pod是啥问题吗？"

echo "=== login ==="
curl -ks -o /dev/null -w 'login=%{http_code}\n' -c $JAR -H "Origin: $H" -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$PASSW\"}" "$H/api/v1/auth/login"
CSRF=$(awk '/__Host-aegisops_csrf/{print $7}' $JAR)
echo "csrf_len=${#CSRF}"

echo "=== query bytes sanity (expect 4-5 utf-8 sequences, CJK) ==="
printf '%s' "$QE" | xxd | head -2

echo "=== agent_context_window BEFORE ==="
curl -ks -b $JAR -H "Origin: $H" "$H/api/v1/settings/ops" | python3 -c '
import json,sys
for it in json.load(sys.stdin)["data"]["items"]:
    if it["key"]=="agent_context_window": print("key=agent_context_window value=%s hasValue=%s default=%s"%(it["value"],it["hasValue"],it["default"]))
'

echo "=== PATCH agent_context_window=262144 ==="
curl -ks -b $JAR -H "Origin: $H" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -X PATCH -d '{"agent_context_window":262144}' "$H/api/v1/settings/ops" \
  -o /tmp/fix_patch.json -w 'patch=%{http_code}\n'
head -c 300 /tmp/fix_patch.json; echo

echo "=== agent_context_window AFTER ==="
curl -ks -b $JAR -H "Origin: $H" "$H/api/v1/settings/ops" | python3 -c '
import json,sys
for it in json.load(sys.stdin)["data"]["items"]:
    if it["key"]=="agent_context_window": print("key=agent_context_window value=%s hasValue=%s default=%s updatedAt=%s"%(it["value"],it["hasValue"],it["default"],it.get("updatedAt")))
'

echo "=== cluster ==="
curl -ks -b $JAR -H "Origin: $H" "$H/api/v1/clusters" -o /tmp/fix_cl.json -w 'clusters=%{http_code}\n'
CID=$(grep -o '"id":"[a-f0-9-]*"' /tmp/fix_cl.json | head -1 | sed 's/"id":"//;s/"//')
echo "cluster=$CID"

echo "=== new session ==="
SID="fix-$(date +%s)"
curl -ks -b $JAR -H "Origin: $H" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$SID\",\"clusterId\":\"$CID\",\"name\":\"fix verify\"}" "$H/api/v1/sessions" -o /tmp/fix_sess.json -w 'create=%{http_code}\n'
head -c 120 /tmp/fix_sess.json; echo

echo "=== send message ==="
python3 - "$H" "$SID" "$JAR" "$CSRF" "$QE" <<'PYEOF'
import json,sys,urllib.request,urllib.error
h=sys.argv[1]; sid=sys.argv[2]; jar=sys.argv[3]; csrf=sys.argv[4]; q=sys.argv[5]
cookies={}
for line in open(jar):
    p=line.split()
    if len(p)>=7 and p[5]=="#HttpOnly_": pass
import http.cookiejar
cj=http.cookiejar.MozillaCookieJar(jar); cj.load(ignore_discard=True,ignore_expires=True)
op=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
data=json.dumps({"query":q}).encode("utf-8")
req=urllib.request.Request(h+"/api/v1/sessions/"+sid+"/send-message",data=data,method="POST")
req.add_header("Content-Type","application/json")
req.add_header("Origin",h)
try:
    r=op.open(req,timeout=20)
    print("send=%d %s"%(r.status,r.read(160).decode("utf-8","replace")))
except urllib.error.HTTPError as e:
    print("send_err=%d %s"%(e.code,e.read(200).decode("utf-8","replace")))
PYEOF

echo "=== stream (max 300s) ==="
curl -ks -b $JAR -H "Origin: $H" -N --max-time 300 "$H/api/v1/sessions/$SID/stream" -o /tmp/fix_stream.txt -w 'stream=%{http_code}\n'
echo "stream_bytes=$(wc -c < /tmp/fix_stream.txt)"
echo "=== compaction/error markers ==="
grep -aE 'compaction|insufficient|narrower|"type":"error"|"status":"failed"|turn.completed.*completed\[^]]*"status":"completed"|"status":"completed"' /tmp/fix_stream.txt | tail -15 || echo "(none)"
echo "=== turn/session lifecycle ==="
grep -aoE '"event_type":"(turn.completed|session.completed|session.failed|turn.failed)"[^}]*' /tmp/fix_stream.txt | tail -8
echo "=== answer/usage tail ==="
grep -aE '"type":"answer"|"type":"state"|"type":"error"' /tmp/fix_stream.txt | tail -6
echo "DONE"
