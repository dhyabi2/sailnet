package net.sailnet.app

import android.app.Activity
import android.app.Application
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import androidx.preference.PreferenceManager
import net.sailnet.mobile.Mobile

/**
 * Out of sight, the tunnel idles.
 *
 * When no Sailnet screen is visible (another app in front, or the screen
 * off) the client closes its circuit and stops pinging, rotating and paying;
 * the tunnel stays up and drops traffic, so nothing leaves outside it and
 * nothing is metered. Opening the app resumes it. The short delay keeps a
 * rotation or a hop between Sailnet's own screens from counting as leaving.
 */
class SailApp : Application(), Application.ActivityLifecycleCallbacks {
    private val main = Handler(Looper.getMainLooper())
    private val worker = java.util.concurrent.Executors.newSingleThreadExecutor() // pause and resume land in order
    private var visible = 0
    private val pause = Runnable {
        // Guests on the hotspot use the tunnel while this phone sleeps.
        if (visible == 0 && pauseWhenHidden() && Share.mode(this) == "off") setPaused(true)
    }

    override fun onCreate() {
        super.onCreate()
        registerActivityLifecycleCallbacks(this)
    }

    private fun pauseWhenHidden() =
        PreferenceManager.getDefaultSharedPreferences(this).getBoolean("pause_hidden", true)

    private fun setPaused(p: Boolean) {
        // Pausing asks the entry what is left of the anchor (a round trip),
        // so it never runs on the main thread.
        worker.execute {
            try { if (p) Mobile.pause() else Mobile.resume() } catch (_: Exception) {}
            main.post { SailVpnService.showPaused(p) }
        }
    }

    override fun onActivityStarted(a: Activity) {
        visible++
        main.removeCallbacks(pause)
        if (visible == 1) setPaused(false)
    }

    override fun onActivityStopped(a: Activity) {
        visible = maxOf(0, visible - 1)
        if (visible == 0) main.postDelayed(pause, 1500)
    }

    override fun onActivityCreated(a: Activity, b: Bundle?) {}
    override fun onActivityResumed(a: Activity) {}
    override fun onActivityPaused(a: Activity) {}
    override fun onActivitySaveInstanceState(a: Activity, b: Bundle) {}
    override fun onActivityDestroyed(a: Activity) {}
}
