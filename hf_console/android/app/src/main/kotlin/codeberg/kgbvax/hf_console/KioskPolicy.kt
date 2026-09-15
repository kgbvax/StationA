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
 * first and only logs. Every policy call logs its result so provisioning
 * problems are diagnosable from `adb logcat -s KioskPolicy` alone.
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
     * Applies the kiosk policies. No-op (logged) when not device owner.
     */
    fun enable() {
        if (!isDeviceOwner()) {
            Log.i(TAG, "enable: skipped — app is not device owner")
            return
        }

        val keyguardOff = dpm.setKeyguardDisabled(admin, true)
        if (keyguardOff) {
            Log.i(TAG, "setKeyguardDisabled(true) -> true")
        } else {
            Log.w(
                TAG,
                "setKeyguardDisabled(true) -> false — a secure lock credential (PIN/pattern/password) " +
                    "is still set. Remove it (Settings -> Security -> Screen lock -> None) and restart the app."
            )
        }

        dpm.setLockTaskPackages(admin, arrayOf(context.packageName))
        Log.i(TAG, "setLockTaskPackages(${context.packageName}) -> ok")

        val home = IntentFilter(Intent.ACTION_MAIN).apply {
            addCategory(Intent.CATEGORY_HOME)
        }
        dpm.addPersistentPreferredActivity(
            admin,
            home,
            ComponentName(context, MainActivity::class.java)
        )
        Log.i(TAG, "addPersistentPreferredActivity(ACTION_MAIN/CATEGORY_HOME -> MainActivity) -> ok")

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            dpm.setLockTaskFeatures(admin, kioskLockTaskFeatures)
            Log.i(TAG, "setLockTaskFeatures(0x${kioskLockTaskFeatures.toString(16)}) -> ok")
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

        val keyguardBack = dpm.setKeyguardDisabled(admin, false)
        Log.i(TAG, "setKeyguardDisabled(false) -> $keyguardBack")

        dpm.setLockTaskPackages(admin, arrayOf<String>())
        Log.i(TAG, "setLockTaskPackages(<empty>) -> ok")

        dpm.clearPackagePersistentPreferredActivities(admin, context.packageName)
        Log.i(TAG, "clearPackagePersistentPreferredActivities(${context.packageName}) -> ok")

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            dpm.setLockTaskFeatures(admin, allLockTaskFeatures)
            Log.i(TAG, "setLockTaskFeatures(all) -> ok")
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
