# PlatformIO pre-build script: stamps the firmware with the git commit it was
# built from, published as /meta device.firmware on both slots. A build from a
# stale or uncommitted checkout is then visible on the bus (2026-10-12: a
# parallel flash from an old base silently reverted a fix — the PLC had no way
# to say what it was running).
import subprocess

Import("env")  # noqa: F821 — injected by PlatformIO


def git(*args):
    try:
        return subprocess.check_output(
            ["git", *args], cwd=env["PROJECT_DIR"], stderr=subprocess.DEVNULL, text=True
        ).strip()
    except Exception:
        return ""


version = git("describe", "--always", "--dirty", "--abbrev=7") or "unknown"
env.Append(CPPDEFINES=[("FW_VERSION", env.StringifyMacro(version))])  # noqa: F821
print("m5stamp-hf-ctrl FW_VERSION =", version)
