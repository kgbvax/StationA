# hf_console — Android kiosk provisioning

Manual device-owner provisioning for the shack tablet: after this, the console
is visible immediately on wake/boot, with **no lock screen in between**.

One device, provisioned over adb. No EMM, no zero-touch. Device owner is a
one-way door: it can only be removed by this app ([Escape hatch](#escape-hatch))
or a factory reset, so read this once before starting.

## Prerequisites

- Android 9.0 (API 27) or newer.
- The device must have **no Google account** and **no secure lock credential**
  (PIN / pattern / password) at provisioning time. Both make
  `dpm set-device-owner` and `setKeyguardDisabled` fail — see
  [Troubleshooting](#troubleshooting).
- ADB enabled on the device, connected to the workstation.

## Steps

1. **Factory reset** the device.

2. **Walk through the setup wizard** without adding any account and without
   setting a PIN, pattern or password. Skip Wi-Fi if it prompts for an account
   later. "Skip" / "Not now" every account screen.

3. **Enable developer options**: Settings → About tablet → tap "Build number"
   7×. Then Settings → Developer options → enable **USB debugging**.

4. **Install the app**:

   ```bash
   adb install build/app/outputs/flutter-apk/app-release.apk
   ```

   (Build it first with `flutter build apk --release` in `hf_console/`.)

5. **Set device owner**:

   ```bash
   adb shell dpm set-device-owner codeberg.kgbvax.hf_console/.KioskAdminReceiver
   ```

   Expected output:

   ```
   Success: Device owner set to component 'codeberg.kgbvax.hf_console/codeberg.kgbvax.hf_console.KioskAdminReceiver'
   ```

   This must come **before** the first launch of the app. It only succeeds
   while no accounts and no lock credential exist, and only immediately after
   setup (no other users/profiles on the device).

6. **Launch the app once** and confirm keyguard disabling, and set the screen
   timeout to 1 h of no touch input (raw setting; survives reboots, survives
   the Settings UI capping at 30 min):

   ```bash
   adb shell am start -n codeberg.kgbvax.hf_console/.MainActivity
   adb shell settings put system screen_off_timeout 3600000
   adb logcat -s KioskPolicy Kiosk
   ```

   Expected lines:

   ```
   KioskPolicy: setKeyguardDisabled(true) -> true
   KioskPolicy: setLockTaskPackages(codeberg.kgbvax.hf_console) -> ok
   KioskPolicy: addPersistentPreferredActivity(ACTION_MAIN/CATEGORY_HOME -> MainActivity) -> ok
   Kiosk:       startLockTask() -> ok
   ```

   If `setKeyguardDisabled(true)` logs `-> false`, a lock credential is still
   set — see troubleshooting. Every policy call logs its result under the
   `KioskPolicy` / `Kiosk` tags, so provisioning problems are diagnosable from
   `adb logcat` alone.

7. **Verify**: press the power button to lock the screen, press it again to
   wake. The console must be visible immediately, with no keyguard step. Also
   press the home gesture/button: the console must come back (it is now the
   home activity). After 1 h without touch input the display turns off; a
   power-button press wakes straight back into the console.

The device now boots straight into the console, in lock task, keyguard off.

## Escape hatch

Device owner cannot be removed from the device settings. The app implements a
deliberate, non-accidental exit — on the **top-left screen corner** (the region
within 15 % of the screen width and height):

- **Long-press ≥ 3 s**, or
- **7 taps within 3.5 s**,

then confirm the "Exit kiosk mode" dialog. That calls `stopLockTask()` and
reverses all policies (keyguard back on, lock task cleared, home activity
deregistered).

The follow-up dialog **"Release device owner?"** requires typing `RELEASE`.
Releasing ownership means kiosk mode can only be re-enabled by repeating the
full provisioning from a factory reset. Don't tap through it.

## Troubleshooting

### `set-device-owner` fails: account present

Symptom:

```
Not allowed to set the device owner because there are already some accounts on the device
```

Even a single Google account added in the wizard blocks provisioning. Fix
without a reset: Settings → Passwords & accounts → remove every account,
then retry step 5. If the account refuses to leave (device admin leftovers,
work profile), factory-reset and redo the wizard, skipping all account steps.

### `setKeyguardDisabled(true) -> false`: lock credential set

Symptom: step 5 succeeds, but the log shows
`setKeyguardDisabled(true) -> false` and the lock screen still appears on wake.
A PIN / pattern / password is set on the device; Android refuses to disable the
keyguard while a secure credential exists. Fix: Settings → Security → Screen
lock → **None**, then restart the app so `enable()` runs again — `enable()`
runs on every `onCreate`:

```bash
adb reboot   # see note below — am force-stop does not stop the device owner
adb wait-for-device
adb shell am start -n codeberg.kgbvax.hf_console/.MainActivity
adb logcat -s KioskPolicy
```

> **Note:** once the app is device owner, `adb shell am force-stop` silently
> does nothing (Android protects the owner app against adb; observed on
> Android 16). A reboot is the reliable restart — with device owner +
> persistent home set, the console comes back up on its own after boot.

If `dpm set-device-owner` itself refuses because of an existing credential,
factory-reset and redo the wizard without setting one.

### Other failures

`dpm set-device-owner` also fails when the device has more than one user or a
work profile, or when an account exists anywhere on it. Keep the device
single-user and account-free; when in doubt, factory-reset and follow the
steps in order (provisioning must happen before first app launch).

## Development machines

On a device (or emulator) without device-owner provisioning the app runs
normally: `KioskPolicy` checks `isDeviceOwnerApp()` before every call, logs
`not device owner — kiosk mode off`, and nothing is locked or policy-managed.
The HOME intent filter may make Android ask which launcher to use when
pressing home on such a device — pick the console or the stock launcher freely;
nothing persists.
