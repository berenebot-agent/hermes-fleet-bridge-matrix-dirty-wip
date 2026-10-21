"""Local regression checks for template pruning; no Docker or network."""
import os
from pathlib import Path
import subprocess
import tempfile


script = Path(__file__).resolve().parents[1] / "bake-fleet-template.sh"
with tempfile.TemporaryDirectory() as tmp:
    root = Path(tmp)
    source = root / "source"
    templates = root / "templates"
    templates.mkdir()
    for name in ("installs/id/environments/id/workspace/tools/python-3.14-linux-x64/bin",
                 "installs/id/environments/id/workspace/agent",
                 "installs/id/environments/id/venv/lib", "cache/uv", "plugins"):
        path = source / "data" / name
        path.mkdir(parents=True)
        (path / "keep").write_text("fixture")
    (source / "data/config.yaml").write_text("plugins: {}\n")
    (source / "data/installs/id/environments/id/workspace/tools/terminal_tool.py").write_text("keep source")
    (source / "data/cache/unrelated").write_text("keep")
    (source / ".env").write_text("PROVIDER_KEY=fixture\n")
    (source / "docker-compose.yml").write_text("services: {}\n")
    env = {**os.environ, "FLEET_TEMPLATE_DIR": str(templates)}
    subprocess.run([str(script), str(source), "basic"], env=env, check=True)
    data = templates / "basic/data"
    assert not (data / "cache/uv").exists()
    tools = data / "installs/id/environments/id/workspace/tools"
    assert os.readlink(tools / "python-3.14-linux-x64") == "/opt/hermes/tools/python-3.14-linux-x64"
    assert (tools / "terminal_tool.py").read_text() == "keep source"
    assert (data / "installs/id/environments/id/venv/lib/keep").read_text() == "fixture"
    assert (data / "installs/id/environments/id/workspace/agent/keep").exists()
    assert (data / "cache/unrelated").exists()
    assert (data / "plugins/keep").exists()
    assert (source / "data/cache/uv/keep").exists(), "source must not be pruned"

    binary = root / "bin"
    binary.mkdir()
    docker = binary / "docker"
    docker.write_text('#!/bin/sh\ncase "$*" in\n*logs*) echo "✓ Installing Python dependencies";;\n*down*) exit "${FAIL_DOWN:-0}";;\nesac\n')
    docker.chmod(0o700)
    env["PATH"] = str(binary) + os.pathsep + env["PATH"]
    (data / "cache/uv").mkdir()
    result = subprocess.run([str(script), "--warm", "basic"], env={**env, "FAIL_DOWN": "1"})
    assert result.returncode != 0
    assert (data / "cache/uv").exists(), "failed shutdown must not prune"
    subprocess.run([str(script), "--warm", "basic"], env=env, check=True)
    assert not (data / "cache/uv").exists()
    result = subprocess.run([str(script), "--warm", ".."], env=env)
    assert result.returncode != 0
print("template pruning checks passed")
