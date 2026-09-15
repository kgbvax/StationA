package codeberg.kgbvax.hf_console

import android.app.AlertDialog
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.text.Editable
import android.text.TextWatcher
import android.util.Log
import android.view.MotionEvent
import android.view.WindowManager
import android.widget.EditText
import io.flutter.embedding.android.FlutterActivity

/**
 * Kiosk entry point. When the app is device owner (see PROVISIONING.md),
 * onCreate applies the kiosk policies via [KioskPolicy] and starts lock task,
 * so after boot / wake the console is the first thing on screen with no
 * keyguard in between.
 *
 * Escape hatch (deliberate, non-accidental): a >= 3 s long-press in the
 * top-left screen corner, or 7 taps in that corner within 3.5 s, opens a
 * confirmation dialog to leave kiosk mode. Releasing device ownership sits
 * behind a second, typed confirmation.
 */
class MainActivity : FlutterActivity() {

    private lateinit var kiosk: KioskPolicy

    private val uiHandler = Handler(Looper.getMainLooper())

    // Escape-hatch tuning. The corner is the region within cornerFraction of
    // both screen dimensions, measured from the top-left origin.
    private val cornerFraction = 0.15f
    private val cornerLongPressMs = 3_000L
    private val cornerTapsNeeded = 7
    private val cornerTapWindowMs = 3_500L

    private var cornerDown = false
    private var cornerTapCount = 0
    private var cornerTapWindowStart = 0L
    private var exitDialogShowing = false

    private val cornerLongPressRunnable = Runnable {
        if (cornerDown) {
            Log.i(TAG, "escape hatch: corner long-press")
            promptExitKiosk()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        kiosk = KioskPolicy(this)
        super.onCreate(savedInstanceState)

        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)

        if (kiosk.isDeviceOwner()) {
            kiosk.enable()
            try {
                startLockTask()
                Log.i(TAG, "startLockTask() -> ok")
            } catch (e: Exception) {
                Log.w(TAG, "startLockTask() -> failed: $e")
            }
        } else {
            Log.i(TAG, "not device owner — kiosk mode off")
        }
    }

    override fun dispatchTouchEvent(ev: MotionEvent): Boolean {
        // Observed before the Flutter view sees the event; the Flutter UI is
        // unaffected. dispatchTouchEvent sees every touch regardless of which
        // widget it lands on.
        observeEscapeGesture(ev)
        return super.dispatchTouchEvent(ev)
    }

    private fun observeEscapeGesture(ev: MotionEvent) {
        val inCorner = isInEscapeCorner(ev.x, ev.y)
        when (ev.actionMasked) {
            MotionEvent.ACTION_DOWN -> if (inCorner) {
                cornerDown = true
                uiHandler.postDelayed(cornerLongPressRunnable, cornerLongPressMs)
            }

            MotionEvent.ACTION_MOVE ->
                if (cornerDown && !inCorner) cancelCornerPress()

            MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> {
                if (cornerDown) cancelCornerPress()
                if (ev.actionMasked == MotionEvent.ACTION_UP && inCorner) registerCornerTap()
            }
        }
    }

    private fun cancelCornerPress() {
        cornerDown = false
        uiHandler.removeCallbacks(cornerLongPressRunnable)
    }

    private fun registerCornerTap() {
        val now = SystemClock.elapsedRealtime()
        if (now - cornerTapWindowStart > cornerTapWindowMs) {
            cornerTapCount = 0
            cornerTapWindowStart = now
        }
        cornerTapCount++
        if (cornerTapCount >= cornerTapsNeeded) {
            cornerTapCount = 0
            Log.i(TAG, "escape hatch: $cornerTapsNeeded corner taps")
            promptExitKiosk()
        }
    }

    private fun isInEscapeCorner(x: Float, y: Float): Boolean {
        val dm = resources.displayMetrics
        return x <= dm.widthPixels * cornerFraction && y <= dm.heightPixels * cornerFraction
    }

    private fun promptExitKiosk() {
        if (exitDialogShowing) return
        exitDialogShowing = true
        AlertDialog.Builder(this)
            .setTitle("Exit kiosk mode")
            .setMessage("Leave kiosk mode and re-enable the lock screen?")
            .setPositiveButton("Exit") { _, _ -> exitKiosk() }
            .setNegativeButton("Stay") { _, _ -> exitDialogShowing = false }
            .setOnCancelListener { exitDialogShowing = false }
            .show()
    }

    private fun exitKiosk() {
        try {
            stopLockTask()
            Log.i(TAG, "stopLockTask() -> ok")
        } catch (e: Exception) {
            Log.w(TAG, "stopLockTask() -> failed: $e")
        }
        kiosk.disable()
        exitDialogShowing = false
        promptReleaseDeviceOwner()
    }

    /**
     * Separate, deliberate second step: releasing device ownership can only be
     * undone by factory reset + re-provisioning, so it needs a typed
     * confirmation, not a tap.
     */
    private fun promptReleaseDeviceOwner() {
        if (!kiosk.isDeviceOwner()) return

        val input = EditText(this).apply { setSingleLine(true) }
        val dialog = AlertDialog.Builder(this)
            .setTitle("Release device owner?")
            .setMessage(
                "This removes kiosk provisioning permanently. Re-enabling kiosk mode " +
                    "requires a factory reset and device-owner setup again. " +
                    "Type $RELEASE_WORD to confirm."
            )
            .setView(input)
            .setPositiveButton("Release") { _, _ -> kiosk.release() }
            .setNegativeButton("Keep", null)
            .show()

        val button = dialog.getButton(AlertDialog.BUTTON_POSITIVE)
        button.isEnabled = false
        input.addTextChangedListener(object : TextWatcher {
            override fun beforeTextChanged(s: CharSequence?, a: Int, b: Int, c: Int) {}
            override fun onTextChanged(s: CharSequence?, a: Int, b: Int, c: Int) {
                button.isEnabled = s?.toString() == RELEASE_WORD
            }
            override fun afterTextChanged(s: Editable?) {}
        })
    }

    companion object {
        private const val TAG = "Kiosk"
        private const val RELEASE_WORD = "RELEASE"
    }
}
