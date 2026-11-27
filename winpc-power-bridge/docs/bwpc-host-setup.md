# bwpc host setup for the 75AUTO remote start

The 75AUTO Start in hf_console needs a logged-on Windows desktop on bwpc. winpc-power-bridge starts AetherSDR, DAX, WSJT-X and 75auto on that desktop. This runbook sets up bwpc so that a cold boot ends at a locked desktop with the agent online. The operator does these steps at bwpc, because they need the Windows password.

## 1. Automatic logon

Use Sysinternals Autologon. It stores the password as an encrypted LSA secret, not as plain text in the registry.

1. Download Autologon from the Microsoft Sysinternals site.
2. Run `Autologon64.exe` as administrator.
3. Enter the user name, the domain (the PC name) and the password, then click Enable.
4. Restart bwpc, and make sure that it logs on without input.

To turn it off again, run Autologon and click Disable.

## 2. Lock the screen at logon

Create a scheduled task that locks the desktop right after the logon. The programs keep running on the locked desktop.

1. Open Task Scheduler and create a task named `lock-at-logon`.
2. Trigger: At log on, for the same user.
3. Action: start the program `rundll32.exe` with the arguments `user32.dll,LockWorkStation`.
4. Settings: allow the task to run on demand, and do not stop it after a time limit.

## 3. Program list of winpc-power-bridge

Fill the `[[program]]` list in `%USERPROFILE%\winpc-power-bridge\config.toml` with the real paths on bwpc. `config.example.toml` has the template. Use these names, because the powerseq chain steps use them: `aethersdr`, `dax`, `wsjtx`, `75auto`.

- `aethersdr`: check `tcp` on its rigctl port, probe `f`, a number as the reply pattern.
- `dax`: check `process`, `restart_owned = true`.
- `wsjtx`: the patched WSJT-X, check `process`, `restart_owned = true`.
- `75auto`: check `none`, with the working folder that holds `75auto.toml`.

Restart the agent task after the edit.

## 4. 75auto configuration

In `75auto.toml` on bwpc, make sure that `[remote] enabled = true` and `commands = "operate"` are set, and that `[start] auto_start` is `false`. If `auto_start` is on, 75auto starts the last session by itself and the remote start is refused.

## 5. Wake-on-LAN

powerseq wakes bwpc with a magic packet and a TCP connect to port 3389. The TCP connect wakes bwpc from Modern Standby. A wake from a full shutdown needs wake-on-LAN in the firmware (UEFI) and in the network adapter settings ("Wake on Magic Packet"). Turn both on.

## 6. Bench check

Do these checks once after the setup, with the operator at the station.

1. Reboot bwpc. It ends at a locked desktop, winpc-power-bridge is online, and no chain program runs.
2. With AetherSDR started first and no SmartSDR window, `ensure_program dax` gives DAX audio in WSJT-X without manual action.
3. AetherSDR answers rigctl `f` with a frequency, and WSJT-X CAT works through it. With another client in the last GUI client slot, the check fails.
4. During a session with the screen locked, bwpc stays awake for 90 min. `powercfg /requests` lists the agent under SYSTEM. After the session ends, bwpc sleeps again.
5. Run `schtasks /end` and `schtasks /run` on the agent task. The programs that the agent started keep running.
6. With chain-owned DAX and WSJT-X running, power-cycle the FLEX, then tap Start. Both restart, and WSJT-X gets DAX audio. WSJT-X closes cleanly, with no lock-file dialog at the next start.
7. A 45 min observe session keeps the antenna connected.
8. Shut bwpc down, then start the station from the Station tab. bwpc wakes at the boot step. If it does not, record the gap in `docs/known-issues.md`.
9. A 75AUTO Start from a cold station ends in a running session, and the PA is in standby.
