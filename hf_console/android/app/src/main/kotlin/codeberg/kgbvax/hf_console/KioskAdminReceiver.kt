package codeberg.kgbvax.hf_console

import android.app.admin.DeviceAdminReceiver

/**
 * Device-admin component the system binds to for device-owner provisioning
 * (`adb shell dpm set-device-owner …/.KioskAdminReceiver`). Intentionally
 * empty: it carries no policies of its own (see res/xml/device_admin.xml);
 * all DevicePolicyManager access goes through [KioskPolicy].
 */
class KioskAdminReceiver : DeviceAdminReceiver()
