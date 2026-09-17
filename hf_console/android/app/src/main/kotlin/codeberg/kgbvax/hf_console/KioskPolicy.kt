package codeberg.kgbvax.hf_console

import android.app.admin.DevicePolicyManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.os.Build
import android.util.Log

/**
 * Single wrapper around [DevicePolicyManager] for kiosk mode: keyguard off,
 * this app as the persistent HOME activity, and lock-task whitelisted.
 *
 * All calls degrade gracefully when the app is not device owner (development
 * machine, unprovisioned device): every entry point checks [isDeviceOwner]
 * first and only logs. Every policy call additionally goes through [attempt],
 * because owner record and admin activation can drift apart — installing a
 * build whose manifest lacks the receiver deactivates the admin, and a later
 * kiosk build then draws SecurityException on every DPM call. Kiosk features
 * may degrade to log lines; the console must still start. Results are logged
 * so provisioning problems are diagnosable from
 * `adb logcat -s KioskPolicy` alone.
 */
class KioskPolicy(private val context: Context) {

    val admin: ComponentName = ComponentName(context, KioskAdminReceiver::class.java)

    private val dpm: DevicePolicyManager =
        context.getSystemService(Context.DEVICE_POLICY_SERVICE) as DevicePolicyManager

    /**
     * Lock-task features that stay available while locked. SYSTEM_INFO keeps
     * the clock/status area visible in the corner of the screen. Change here
     * if the kiosk needs more (e.g. HOME_BUTTON for a nav-bar home affordance).
     */
    private val kioskLockTaskFeatures: Int =
        DevicePolicyManager.LOCK_TASK_FEATURE_SYSTEM_INFO

    /**
     * Feature set restored by [disable] — everything, i.e. stock behaviour
     * outside kiosk mode. (HOME_BUTTON/RECENTS of older API levels were
     * folded into HOME/OVERVIEW; the newer KEYGUARD and
     * BLOCK_ACTIVITY_START_IN_TASK are not part of the stock default set.)
     */
    private val allLockTaskFeatures: Int =
        DevicePolicyManager.LOCK_TASK_FEATURE_HOME or
            DevicePolicyManager.LOCK_TASK_FEATURE_OVERVIEW or
            DevicePolicyManager.LOCK_TASK_FEATURE_GLOBAL_ACTIONS or
            DevicePolicyManager.LOCK_TASK_FEATURE_NOTIFICATIONS or
            DevicePolicyManager.LOCK_TASK_FEATURE_SYSTEM_INFO

    fun isDeviceOwner(): Boolean =
        dpm.isDeviceOwnerApp(context.packageName)

    /**
     * Runs one device-policy call, degrading to a log line when the system
     * rejects it. SecurityException here means the owner record is intact but
     * the admin is no longer active (component was absent from an intervening
     * install) — reactivate with
     * `adb shell dpm set-active-admin <pkg>/.KioskAdminReceiver`.
     */
    private fun policyCall(what: String, block: () -> Unit) {
        try {
            block()
            Log.i(TAG, "$what -> ok")
        } catch (e: Exception) {
            Log.w(TAG, "$what -> failed: $e")
        }
    }

    /**
     * Applies the kiosk policies. No-op (logged) when not device owner.
     */
    fun enable() {
        if (!isDeviceOwner()) {
            Log.i(TAG, "enable: skipped — app is not device owner")
            return
        }

        val keyguardOff = try {
            dpm.setKeyguardDisabled(admin, true)
        } catch (e: Exception) {
            Log.w(TAG, "setKeyguardDisabled(true) -> failed: $e")
            null
        }
        when (keyguardOff) {
            true -> Log.i(TAG, "setKeyguardDisabled(true) -> true")
            false -> Log.w(
                TAG,
                "setKeyguardDisabled(true) -> false — a secure lock credential (PIN/pattern/password) " +
                    "is still set. Remove it (Settings -> Security -> Screen lock -> None) and restart the app."
            )
            null -> {} // already logged above
        }

        policyCall("setLockTaskPackages(${context.packageName})") {
            dpm.setLockTaskPackages(admin, arrayOf(context.packageName))
        }

        val home = IntentFilter(Intent.ACTION_MAIN).apply {
            addCategory(Intent.CATEGORY_HOME)
        }
        policyCall("addPersistentPreferredActivity(ACTION_MAIN/CATEGORY_HOME -> MainActivity)") {
            dpm.addPersistentPreferredActivity(
                admin,
                home,
                ComponentName(context, MainActivity::class.java)
            )
        }

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            policyCall("setLockTaskFeatures(0x${kioskLockTaskFeatures.toString(16)})") {
                dpm.setLockTaskFeatures(admin, kioskLockTaskFeatures)
            }
        } else {
            Log.i(TAG, "setLockTaskFeatures: skipped — requires API 28")
        }
    }

    /**
     * Reverses everything [enable] applied. No-op (logged) when not device owner.
     */
    fun disable() {
        if (!isDeviceOwner()) {
            Log.i(TAG, "disable: skipped — app is not device owner")
            return
        }

        try {
            val keyguardBack = dpm.setKeyguardDisabled(admin, false)
            Log.i(TAG, "setKeyguardDisabled(false) -> $keyguardBack")
        } catch (e: Exception) {
            Log.w(TAG, "setKeyguardDisabled(false) -> failed: $e")
        }

        policyCall("setLockTaskPackages(<empty>)") {
            dpm.setLockTaskPackages(admin, arrayOf<String>())
        }

        policyCall("clearPackagePersistentPreferredActivities(${context.packageName})") {
            dpm.clearPackagePersistentPreferredActivities(admin, context.packageName)
        }

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            policyCall("setLockTaskFeatures(all)") {
                dpm.setLockTaskFeatures(admin, allLockTaskFeatures)
            }
        }
    }

    /**
     * Removes device ownership entirely. After this, kiosk mode can only be
     * re-enabled by repeating the full provisioning (factory reset + dpm
     * set-device-owner) — see PROVISIONING.md.
     */
    fun release() {
        Log.w(
            TAG,
            "clearDeviceOwnerApp(${context.packageName}) — releasing device owner; " +
                "kiosk mode requires factory reset + re-provisioning after this"
        )
        dpm.clearDeviceOwnerApp(context.packageName)
    }

    companion object {
        private const val TAG = "KioskPolicy"
    }
}
