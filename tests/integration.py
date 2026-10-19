#!/usr/bin/env python3
"""Opt-in real image test. Only generated scratch projects, keys and local fake inference.

Build the application image first. --keep retains the scratch bridge for a browser
check; run the recorded cleanup command afterwards. No live fleet is mounted.
"""
import argparse
import base64
import hashlib
import http.server
import json
import os
import pathlib
import re
import secrets
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

REPO = pathlib.Path(__file__).resolve().parents[1]
HERMES = "nousresearch/hermes-agent@sha256:9774f4f39a9bb8c2f68ce728ed5e99ddbad282163be56764afacf88ed952b784"
REDACTIONS = []
PIXEL = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a5XsAAAAASUVORK5CYII=")

def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]

def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True)
    if result.returncode:
        detail = result.stderr[:2000]
        for secret in REDACTIONS:
            detail = detail.replace(secret, "[redacted]")
        raise RuntimeError("isolated Docker operation failed: " + args[0] + ": " + detail)
    return result.stdout

def wait_for(check, timeout=180):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            value = check()
            if value:
                return value
        except (urllib.error.URLError, RuntimeError, KeyError, FileNotFoundError):
            pass
        time.sleep(0.5)
    raise RuntimeError("isolated check timed out")

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--keep", action="store_true")
    parser.add_argument("--image", default="hermes-fleet-bridge-matrix:local")
    args = parser.parse_args()
    suffix = secrets.token_hex(4)
    root = REPO / "tests/logs" / ("integration-" + suffix)
    root.mkdir(parents=True, mode=0o700)
    member = root / "iris"
    home = member / "data"
    home.mkdir(parents=True, mode=0o700)
    data = root / "bridge-data"
    data.mkdir(mode=0o700)
    project = "hfb_matrix_it_" + suffix
    bridge = project + "_bridge"
    frontend, internal, api_port = free_port(), free_port(), free_port()
    calls = []
    model_calls = []
    lock = threading.Lock()

    class Provider(http.server.BaseHTTPRequestHandler):
        def handle(self):
            try:
                super().handle()
            except (BrokenPipeError, ConnectionResetError):
                # A stopped run intentionally disconnects from fake inference.
                pass

        def log_message(self, *unused):
            pass

        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"object": "list", "data": [{"id": "test-model", "object": "model", "owned_by": "isolated-test"}]}).encode())

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            last = next((m.get("content", "") for m in reversed(body.get("messages", [])) if m.get("role") == "user"), "")
            text = str(last)
            with lock:
                calls.append(text)
                model_calls.append((text,body.get('model')))
            if "slow-restart-check" in text:
                time.sleep(12)
            if 'slow-stop-check' in text:
                time.sleep(12)
            approval='approval-check' in text and not any(m.get('role')=='tool' for m in body.get('messages',[]))
            tool={'id':'call_hfb_approval','type':'function','function':{'name':'terminal','arguments':json.dumps({'command':'rm -rf /tmp/hfb_matrix_approval_nonexistent_'+suffix})}}
            answer = "Integration reply " + hashlib.sha256(text.encode()).hexdigest()[:12]
            if "media-roundtrip-check" in text or "image-roundtrip-check" in text:
                turn_hash = re.search(r"/opt/data/matrix-media/outgoing/([0-9a-f]+)/", text).group(1)
                output = home / "matrix-media/outgoing" / turn_hash
                output.mkdir(parents=True, exist_ok=True)
                (output / "result.txt").write_text("isolated media result\n")
                (output / "pixel.png").write_bytes(PIXEL)
                answer = "Media reply\nMATRIX_FILE: result.txt\nMATRIX_FILE: pixel.png"
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
            self.end_headers()
            if body.get("stream"):
                delta={'role':'assistant','content':answer}
                if approval:
                    delta={'role':'assistant','tool_calls':[dict(tool,index=0)]}
                for choice in [{"delta": delta, "finish_reason": None, "index": 0}, {"delta": {}, "finish_reason": "tool_calls" if approval else "stop", "index": 0}]:
                    self.wfile.write(("data: " + json.dumps({"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": body.get('model','test-model'), "choices": [choice]}) + "\n\n").encode())
                self.wfile.write(b"data: [DONE]\n\n")
            else:
                message={'role':'assistant','content':answer}
                if approval:
                    message={'role':'assistant','content':'','tool_calls':[tool]}
                self.wfile.write(json.dumps({"id": "chatcmpl-test", "object": "chat.completion", "model": body.get('model','test-model'), "choices": [{"index": 0, "message": message, "finish_reason": "tool_calls" if approval else "stop"}], "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}).encode())

    # An internal network permits its own host gateway, not the unrelated docker0
    # gateway. Match Compose labels so normal Compose down owns this scratch network.
    network=project+'_default'
    docker('network','create','--internal','--label','com.docker.compose.project='+project,'--label','com.docker.compose.network=default',network)
    gateway = docker("network", "inspect", network, "--format", "{{(index .IPAM.Config 0).Gateway}}").strip()
    provider = http.server.ThreadingHTTPServer((gateway, 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    provider_port = provider.server_address[1]
    (home / "config.yaml").write_text(f"model:\n  default: test-model\n  provider: custom\n  base_url: http://{gateway}:{provider_port}/v1\nagent:\n  max_turns: 3\n")
    unrelated='UNRELATED_MULTILINE="start\nescaped \\" quote\nAPI_SERVER_HOST=inside-other-value\nlast"\n'
    (home / ".env").write_text(f"OPENAI_API_KEY=isolated-dummy-not-a-live-provider\nOPENAI_BASE_URL=http://{gateway}:{provider_port}/v1\nUNRELATED_PRESERVE=yes\n"+unrelated)
    os.chmod(home / ".env", 0o600)
    compose = member / "docker-compose.yml"
    compose.write_text(f"name: {project}\nservices:\n  hermes:\n    image: {HERMES}\n    command: [gateway, run]\n    mem_limit: 1500m\n    environment:\n      HERMES_UID: '{os.getuid()}'\n      HERMES_GID: '{os.getgid()}'\n    volumes:\n      - ./data:/opt/data\nnetworks:\n  default:\n    internal: true\n")
    members=[member]
    for name in ['lyra','nova']:
        extra=root/name
        (extra/'data').mkdir(parents=True,mode=0o700)
        (extra/'data/config.yaml').write_text((home/'config.yaml').read_text())
        (extra/'data/.env').write_text((home/'.env').read_text())
        os.chmod(extra/'data/.env',0o600)
        (extra/'docker-compose.yml').write_text(compose.read_text().replace('name: '+project+'\n','name: '+project+'_'+name+'\n'))
        members.append(extra)
    password = secrets.token_urlsafe(32)
    REDACTIONS.append(password)
    envfile = root / "bridge.env"
    envfile.write_text(f"PUBLIC_URL=https://chat.test\nMATRIX_SERVER_NAME=chat.test\nADMIN_USERNAME=admin\nADMIN_PASSWORD={password}\nHERMES_DIRS="+','.join(map(str,members))+"\nHERMES_OVERRIDES=" + json.dumps({"iris": {"port": api_port}}) + f"\nLISTEN_ADDR=127.0.0.1:{frontend}\nMATRIX_LISTEN_ADDR=127.0.0.1:{internal}\nIDLE_TIMEOUT=10s\nSTART_TIMEOUT=120s\n")
    os.chmod(envfile, 0o600)
    base = f"http://127.0.0.1:{frontend}"

    def request(method, path, body=None, token=None, content_type="application/json"):
        headers = {"Content-Type": content_type}
        if token:
            headers["Authorization"] = "Bearer " + token
        payload = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(base + path, data=payload, method=method, headers=headers)
        try:
            with urllib.request.urlopen(r, timeout=40) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError("HTTP " + str(error.code)) from None

    def state():
        return json.loads((data / "controller/state.json").read_text())["Members"]["iris"]

    def launch():
        mounts=[]
        for directory in members:
            mounts+=['-v',str(directory)+':'+str(directory)]
        docker("run", "-d", "--name", bridge, "--network", "host", "--user", f"{os.getuid()}:{os.getgid()}", "--group-add", str(os.stat("/var/run/docker.sock").st_gid), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777", "--memory", "3g", "--env-file", str(envfile), "-v", str(data) + ":/data", *mounts, "-v", "/var/run/docker.sock:/var/run/docker.sock", args.image)
        wait_for(lambda: request("GET", "/healthz")["ready"])

    try:
        launch()
        login = request("POST", "/_matrix/client/v3/login", {"type": "m.login.password", "identifier": {"type": "m.id.user", "user": "admin"}, "password": password})
        token = login["access_token"]
        room = state()["Room"]
        escaped_room = urllib.parse.quote(room, safe="")
        assert room in request("GET", "/_matrix/client/v3/joined_rooms", token=token)["joined_rooms"]
        rooms_before=json.loads((data/'controller/state.json').read_text())['Members']
        assert len(rooms_before)==3
        assert all(m['Room'] in request('GET','/_matrix/client/v3/joined_rooms',token=token)['joined_rooms'] for m in rooms_before.values())
        assert (home / ".env").read_text().count("API_SERVER_KEY=") == 1
        assert "UNRELATED_PRESERVE=yes" in (home / ".env").read_text()
        assert unrelated in (home/'.env').read_text()
        try:
            request("POST", "/_matrix/client/v3/register", {"username": "intruder", "password": secrets.token_urlsafe(32), "auth": {"type": "m.login.dummy"}})
            raise AssertionError("public registration open")
        except RuntimeError as error:
            assert str(error) == "HTTP 403"
        try:
            request("GET", "/_synapse/admin/v1/register")
            raise AssertionError("admin endpoint public")
        except RuntimeError as error:
            assert str(error) == "HTTP 404"
        print("PASS bundle login, three visible private agent rooms, closed signup, blocked public admin API", flush=True)

        def send(text, thread=""):
            content = {"msgtype": "m.text", "body": text}
            if thread:
                content["m.relates_to"] = {"rel_type": "m.thread", "event_id": thread, "is_falling_back": True, "m.in_reply_to": {"event_id": thread}}
            return request("PUT", "/_matrix/client/v3/rooms/" + escaped_room + "/send/m.room.message/" + secrets.token_hex(12), content, token)["event_id"]

        def settled(event):
            return next((t for t in (state()["Turns"] or []) if t["ID"] == event and t["Phase"] == "done"), None)

        def react_menu(menu_id, key):
            target = wait_for(lambda: (state().get("Menus") or {}).get(menu_id, {}).get("EventID"))
            content = {"m.relates_to": {"rel_type": "m.annotation", "event_id": target, "key": key}}
            event = request("PUT", "/_matrix/client/v3/rooms/" + escaped_room + "/send/m.reaction/" + secrets.token_hex(12), content, token)["event_id"]
            wait_for(lambda: settled(event))
            return event

        first = send("cold-start-check")
        wait_for(lambda: settled(first))
        wait_for(lambda: not state()["Outbox"])
        messages = request("GET", "/_matrix/client/v3/rooms/" + escaped_room + "/messages?dir=b&limit=100", token=token)["chunk"]
        assert any("Integration reply" in ev.get("content", {}).get("body", "") for ev in messages)
        print("PASS stopped agent cold-wake, actual Hermes run, final Matrix reply", flush=True)

        uploaded = request("POST", "/_matrix/media/v3/upload?filename=input.txt", b"isolated input file\n", token, "text/plain")["content_uri"]
        content = {"msgtype": "m.file", "body": "media-roundtrip-check", "filename": "input.txt", "url": uploaded, "info": {"size": 20, "mimetype": "text/plain"}}
        attachment = request("PUT", "/_matrix/client/v3/rooms/" + escaped_room + "/send/m.room.message/" + secrets.token_hex(12), content, token)["event_id"]
        wait_for(lambda: settled(attachment))
        wait_for(lambda: not state()["Outbox"])
        received = settled(attachment)["Media"]
        assert received["Ready"] and received["SHA256"] == hashlib.sha256(b"isolated input file\n").hexdigest()
        messages = request("GET", "/_matrix/client/v3/rooms/" + escaped_room + "/messages?dir=b&limit=100", token=token)["chunk"]
        returned = next(ev["content"] for ev in messages if ev.get("content", {}).get("msgtype") == "m.file" and ev["content"]["body"] == "result.txt")
        server, media_id = returned["url"].removeprefix("mxc://").split("/", 1)
        download = urllib.request.Request(base + "/_matrix/client/v1/media/download/" + server + "/" + media_id, headers={"Authorization": "Bearer " + token})
        with urllib.request.urlopen(download, timeout=30) as response:
            assert response.read() == b"isolated media result\n"
        returned_image = next(ev["content"] for ev in messages if ev.get("content", {}).get("msgtype") == "m.image" and ev["content"]["body"] == "pixel.png")
        server, media_id = returned_image["url"].removeprefix("mxc://").split("/", 1)
        download = urllib.request.Request(base + "/_matrix/client/v1/media/download/" + server + "/" + media_id, headers={"Authorization": "Bearer " + token})
        with urllib.request.urlopen(download, timeout=30) as response:
            assert response.read() == PIXEL
        uploaded_image = request("POST", "/_matrix/media/v3/upload?filename=pixel.png", PIXEL, token, "image/png")["content_uri"]
        content = {"msgtype": "m.image", "body": "image-roundtrip-check", "filename": "pixel.png", "url": uploaded_image, "info": {"size": len(PIXEL), "mimetype": "image/png"}}
        image_event = request("PUT", "/_matrix/client/v3/rooms/" + escaped_room + "/send/m.room.message/" + secrets.token_hex(12), content, token)["event_id"]
        wait_for(lambda: settled(image_event))
        wait_for(lambda: not state()["Outbox"])
        assert settled(image_event)["Media"]["MIME"] == "image/png"
        print("PASS actual Matrix authenticated file/image input downloads, Hermes text-run attachment contract, output uploads and downloads", flush=True)

        slow = send("slow-restart-check")
        wait_for(lambda: next((t for t in (state()["Turns"] or []) if t["ID"] == slow and t["RunID"]), None))
        docker("restart", "--timeout", "30", bridge)
        wait_for(lambda: request("GET", "/healthz")["ready"])
        wait_for(lambda: settled(slow))
        with lock:
            assert sum("slow-restart-check" in text for text in calls) == 1
        assert state()["Room"] == room
        print("PASS bridge restart during a real Hermes run: no duplicate inference, same room", flush=True)

        with lock:
            count_before_threads = len(calls)
        thread_a = send("thread-a-check", first)
        thread_b = send("thread-b-check", slow)
        wait_for(lambda: settled(thread_a) and settled(thread_b))
        with lock:
            assert len(calls) == count_before_threads
        print("PASS new thread prompts explicitly rejected without inference", flush=True)

        wait_for(lambda: not state()["Outbox"])
        original_session = state()["Sessions"][room]["room"]["ID"]
        fresh = send("!new")
        wait_for(lambda: settled(fresh))
        selected = state()["Rooms"][room]["Selected"]
        assert state()["Sessions"][room][selected]["ID"] != original_session
        wait_for(lambda: not state()["Outbox"])
        resume = send("!resume cold-start-check")
        wait_for(lambda: settled(resume))
        react_menu(resume + "-menu", "1️⃣")
        assert state()["Rooms"][room]["Selected"] == "room"
        assert state()["Sessions"][room]["room"]["ID"] == original_session
        print("PASS non-destructive New/Resume with a Matrix reaction choice", flush=True)

        approval_turn=send('approval-check')
        approval_id=wait_for(lambda:next((t['Approval'] for t in (state()['Turns'] or []) if t['ID']==approval_turn and t['Approval']),None),timeout=90)
        deny=react_menu(approval_turn+'-approval-'+approval_id, '❌')
        wait_for(lambda:settled(deny) and settled(approval_turn))
        print('PASS real Hermes tool approval surfaced in Matrix and exact request denied',flush=True)

        stopping=send('slow-stop-check')
        wait_for(lambda:next((t for t in (state()['Turns'] or []) if t['ID']==stopping and t['RunID']),None))
        queued=send('must-not-run-after-stop')
        stop=send('!stop')
        wait_for(lambda:settled(stop) and settled(stopping) and settled(queued))
        with lock:
            assert not any('must-not-run-after-stop' in text for text in calls)
        print('PASS real run cancellation and queued-turn cancellation in the same conversation',flush=True)

        for command in ["!help", "!models", "!context", "!usage", "!status"]:
            event = send(command)
            wait_for(lambda: settled(event))
        wait_for(lambda: not state()["Outbox"])
        select=send('!model test-model-alternate custom')
        wait_for(lambda:settled(select))
        react_menu(select+'-menu', '1️⃣')
        switched=send('selected-model-check')
        wait_for(lambda:settled(switched))
        with lock:
            assert any('selected-model-check' in text and model=='test-model-alternate' for text,model in model_calls)
        print("PASS menu/status/model scope/context-blocker/usage command paths", flush=True)
        wait_for(lambda: not docker("ps", "-q", "--filter", "label=com.docker.compose.project=" + project).strip(), timeout=90)
        print("PASS authenticated idle checks and Compose down without deleting bind state", flush=True)

        docker('stop','--timeout','30',bridge)
        docker('rm',bridge)
        content=envfile.read_text()
        content=content.replace('HERMES_DIRS='+','.join(map(str,members)),'HERMES_DIRS='+str(member))
        envfile.write_text(content)
        launch()
        after=json.loads((data/'controller/state.json').read_text())['Members']
        assert not after['lyra']['Enabled'] and not after['nova']['Enabled']
        assert all(after[k]['Room']==v['Room'] for k,v in rooms_before.items())
        assert all(m['Room'] in request('GET','/_matrix/client/v3/joined_rooms',token=token)['joined_rooms'] for m in after.values())
        print('PASS removal disables dispatch while retaining all rooms and histories',flush=True)

        metadata = {"base": base, "root": str(root), "bridge": bridge, "compose": str(compose), "room": room}
        (REPO / "tests/logs/browser-fixture.json").write_text(json.dumps(metadata))
        os.chmod(REPO / "tests/logs/browser-fixture.json", 0o600)
        (root / "result.json").write_text(json.dumps({"passed": True, "checks": 10, "root": str(root)}))
        if args.keep:
            print("Scratch bridge retained for browser check; metadata: tests/logs/browser-fixture.json", flush=True)
    except Exception as error:
        logs = subprocess.run(["docker", "logs", "--tail", "30", bridge], capture_output=True, text=True).stdout
        logs = logs.replace(password, "[redacted]")
        (root / "failure.log").write_text(logs)
        os.chmod(root / "failure.log", 0o600)
        print("FAIL", type(error).__name__, str(error), "(sanitized bridge log retained privately)", flush=True)
        raise SystemExit(1) from None
    finally:
        provider.shutdown()
        if not args.keep:
            docker("stop", "--timeout", "30", bridge)
            docker("rm", bridge)
            for directory in members:
                docker("compose", "--file", str(directory/'docker-compose.yml'), "down", "--timeout", "30")

if __name__ == "__main__":
    main()
