#!/usr/bin/env python3
"""Real C-Gate checks in a disposable Docker container with no network access.
Usage: python3 tests/integration.py IMAGE
No host ports, devices, production projects, or registries are modified.
"""
import json
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import tempfile
import time
import uuid
import zipfile

image = sys.argv[1] if len(sys.argv) > 1 else "cgate-fix:runtime"
name = "cgate-integration-" + uuid.uuid4().hex[:8]


def docker(*args, check=True):
    return subprocess.run(["docker", *args], check=check, capture_output=True, text=True)


def request(path, *args):
    out = docker("exec", name, "curl", "-sS", "--max-time", "120", "-w", "\n%{http_code}", *args,
                 "http://127.0.0.1:8980" + path).stdout
    body, status = out.rsplit("\n", 1)
    return int(status), body


def api(command):
    from urllib.parse import quote
    status, body = request("/cgate?cmd=" + quote(command, safe=""))
    assert status == 200, (command, status, body)
    reply = json.loads(body)["response"]
    assert reply and re.match(r"[123]\d\d[ -]", reply[-1]), (command, reply)
    return reply


def ready():
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        try:
            if request("/ready")[0] == 200:
                # A welcome banner cannot be mistaken for this response.
                assert api("noop") == ["200 OK."], "startup response desynchronization"
                return
        except (subprocess.CalledProcessError, AssertionError):
            pass
        time.sleep(.5)
    raise AssertionError("C-Gate did not become ready")


def description(database):
    with sqlite3.connect("file:" + str(database) + "?mode=ro", uri=True) as conn:
        return conn.execute("SELECT description FROM tagged_entity WHERE id=(SELECT tagged_entity_id FROM project)").fetchone()[0]


with tempfile.TemporaryDirectory(prefix="cgate-integration-") as temp:
    data = Path(temp)
    options = data / "options.json"
    options.write_text(json.dumps({"project_name": "HOME", "log_level": "INFO", "cgate_args": "-s"}))
    try:
        docker("run", "-d", "--name", name, "--network", "none", "--add-host",
               "homeassistant.local.hass.io:127.0.0.1", "-v", str(data) + ":/data", image)
        ready()
        api("project stop HOME")
        api("project close HOME")
        empty = api("project list")
        assert json.loads(request("/tag")[1])["state_known"], ("empty project list was not recognized", empty)
        for project in ("AUDIT", "OTHER"):
            api("project new " + project)
            api("project save " + project)
        api("project start AUDIT")
        assert any("project=AUDIT state=started" in r for r in api("project list"))
        candidate = data / "candidate.db"
        candidate.write_bytes((data / "projects/AUDIT/AUDIT.db").read_bytes())
        with sqlite3.connect(candidate) as conn:
            conn.execute("UPDATE tagged_entity SET description='FIXED_UPLOAD_MARKER' WHERE id=(SELECT tagged_entity_id FROM project)")
        status, body = request("/tag/upload", "-F", "project=AUDIT", "-F", "file=@/data/candidate.db")
        assert status == 200, (status, body)
        assert any("FIXED_UPLOAD_MARKER" in r for r in api("dbgetxml //AUDIT")), "C-Gate retained old in-memory data"
        api("project save AUDIT")
        assert description(data / "projects/AUDIT/AUDIT.db") == "FIXED_UPLOAD_MARKER", "save erased uploaded data"
        assert (data / "projects/AUDIT.bak/AUDIT.db").is_file(), "rollback copy missing"
        print("PASS: started project on multiline list safely replaced; subsequent save retains uploaded data", flush=True)

        before = (data / "projects/OTHER/OTHER.db").read_bytes()
        with zipfile.ZipFile(data / "invalid.zip", "w") as archive:
            archive.writestr("OTHER.db", "not a database")
        status, body = request("/tag/upload", "-F", "file=@/data/invalid.zip")
        assert status == 400, (status, body)
        assert (data / "projects/OTHER/OTHER.db").read_bytes() == before
        print("PASS: invalid archive rejected without modifying existing database", flush=True)

        with zipfile.ZipFile(data / "wrapped.zip", "w") as archive:
            archive.write(data / "projects/OTHER/OTHER.db", "OTHER/OTHER.db")
        status, body = request("/tag/upload", "-F", "file=@/data/wrapped.zip")
        assert status == 200, (status, body)
        status, body = request("/tag/backup?project=AUDIT&format=cbz", "-X", "POST", "-D", "/data/backup-headers", "-o", "/data/backup.cbz")
        assert status == 200, (status, body)
        assert "x-cgate-saved: true" in (data / "backup-headers").read_text().lower()
        with zipfile.ZipFile(data / "backup.cbz") as archive:
            assert "AUDIT.db" in archive.namelist() and archive.testzip() is None
        print("PASS: wrapped archive accepted and save-and-backup produced a verified CBZ", flush=True)

        old = docker("exec", name, "pidof", "cgate-web").stdout.strip()
        java = docker("exec", name, "pidof", "java").stdout.strip()
        docker("exec", name, "kill", "-9", old)
        deadline = time.monotonic() + 15
        new = old
        while time.monotonic() < deadline:
            time.sleep(.25)
            result = docker("exec", name, "pidof", "cgate-web", check=False)
            if result.returncode == 0 and result.stdout.strip() != old:
                new = result.stdout.strip()
                break
        assert new != old, "bridge crash was not recovered"
        assert docker("exec", name, "pidof", "java").stdout.strip() == java
        ready()
        print("PASS: bridge crash recovered without restarting Java", flush=True)

        # Simulate migration interrupted after destination mkdir on a prior boot.
        docker("stop", "-t", "10", name)
        (data / "tag/LEGACY").mkdir(parents=True)
        (data / "tag/LEGACY/LEGACY.db").write_bytes(before)
        (data / "projects/LEGACY").mkdir()
        options.write_text(json.dumps({"project_name": "OTHER", "log_level": "INFO", "cgate_args": "-s"}))
        docker("start", name)
        ready()
        states = api("project list")
        assert any("project=OTHER state=started" in r for r in states), states
        assert not any("project=HOME state=started" in r for r in states), states
        assert json.loads(request("/tag")[1])["active"] == "OTHER"
        assert (data / "projects/LEGACY/LEGACY.db").is_file() and not (data / "tag/LEGACY").exists()
        argv = docker("exec", name, "cat", "/proc/1/cmdline").stdout.split("\x00")
        assert argv.count("-s") == 2, argv
        print("PASS: effective project option, literal extra arguments and interrupted migration", flush=True)
    except Exception:
        print(docker("logs", "--tail", "100", name, check=False).stdout, file=sys.stderr)
        raise
    finally:
        docker("rm", "-f", name, check=False)
