package net.sailnet.app

import android.app.Application
import android.app.KeyguardManager
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.os.Handler
import android.os.Looper
import androidx.preference.PreferenceManager
import net.sailnet.mobile.Mobile

/**
 * Screen off, the tunnel idles.
 *
 * When the screen goes off the client closes its circuit and stops pinging,
 * rotating and paying; the tunnel stays up and drops traffic, so nothing
 * leaves outside it and nothing is metered. Unlocking resumes it. Other
 * apps in front of Sailnet keep using the tunnel: pausing whenever Sailnet
 * was out of sight cut off the browser, the one app a VPN is for. The
 * short delay keeps a quick off-and-on of the screen from counting.
 */
class SailApp : Application() {
    private val main = Handler(Looper.getMainLooper())
    private val worker = java.util.concurrent.Executors.newSingleThreadExecutor() // pause and resume land in order
    private val pause = Runnable {
        // Guests on the hotspot use the tunnel while this phone sleeps.
        if (pauseWhenScreenOff() && Share.mode(this) == "off") setPaused(true)
    }

    private val screen = object : BroadcastReceiver() {
        override fun onReceive(c: Context, i: Intent) {
            when (i.action) {
                Intent.ACTION_SCREEN_OFF -> main.postDelayed(pause, 1500)
                // On without a lock screen, or unlocked: resume.
                Intent.ACTION_SCREEN_ON -> if (!getSystemService(KeyguardManager::class.java).isKeyguardLocked) resume()
                Intent.ACTION_USER_PRESENT -> resume()
            }
        }
    }

    override fun onCreate() {
        super.onCreate()
        registerReceiver(screen, IntentFilter().apply {
            addAction(Intent.ACTION_SCREEN_OFF)
            addAction(Intent.ACTION_SCREEN_ON)
            addAction(Intent.ACTION_USER_PRESENT)
        })
    }

    private fun resume() {
        main.removeCallbacks(pause)
        setPaused(false)
    }

    private fun pauseWhenScreenOff() =
        PreferenceManager.getDefaultSharedPreferences(this).getBoolean("pause_hidden", true)

    private fun setPaused(p: Boolean) {
        // Pausing asks the entry what is left of the anchor (a round trip),
        // so it never runs on the main thread.
        worker.execute {
            try { if (p) Mobile.pause() else Mobile.resume() } catch (_: Exception) {}
            main.post { SailVpnService.showPaused(p) }
        }
    }
}
