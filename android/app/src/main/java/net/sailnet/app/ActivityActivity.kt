package net.sailnet.app

import android.app.AppOpsManager
import android.app.usage.NetworkStats
import android.app.usage.NetworkStatsManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.ConnectivityManager
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.widget.TextView
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import net.sailnet.mobile.Mobile
import org.json.JSONArray

/**
 * Which app is using the tunnel, and how much.
 *
 * Android's per-app network statistics (NetworkStatsManager) are read from
 * the moment the tunnel came up. Reading them for other apps needs the
 * "Usage access" permission, which only the user can grant, on the system
 * page this screen opens. While Sailnet is connected every app's traffic
 * goes through the tunnel, so those counters are its tunnel usage. Nothing
 * leaves the phone.
 */
class ActivityActivity : AppCompatActivity() {
    private val ui = Handler(Looper.getMainLooper())
    private lateinit var text: TextView
    private val worker = java.util.concurrent.Executors.newSingleThreadExecutor()
    @Volatile private var stopped = false
    private val label = HashMap<Int, String>()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_text)
        text = findViewById(R.id.text)
        text.textSize = 12f
    }

    override fun onResume() {
        super.onResume()
        stopped = false
        if (hasUsageAccess()) ui.post(refresh) else askPermission()
    }

    override fun onPause() { stopped = true; ui.removeCallbacksAndMessages(null); super.onPause() }

    private fun hasUsageAccess(): Boolean {
        val ops = getSystemService(Context.APP_OPS_SERVICE) as AppOpsManager
        val mode = ops.unsafeCheckOpNoThrow(AppOpsManager.OPSTR_GET_USAGE_STATS, android.os.Process.myUid(), packageName)
        return mode == AppOpsManager.MODE_ALLOWED
    }

    /** Explain, then send the user to Android's own permission page. */
    private fun askPermission() {
        text.text = "Sailnet needs the \"Usage access\" permission to show which apps use the tunnel.\n\nTap Allow, switch on Sailnet on the Android page that opens, then come back."
        AlertDialog.Builder(this)
            .setTitle("Show app activity?")
            .setMessage("To show which apps use the tunnel and how much, Sailnet reads Android's per-app network statistics. Android asks you to grant \"Usage access\" for that on its own settings page. The data stays on the phone and is never sent anywhere.")
            .setCancelable(false)
            .setNegativeButton("Not now") { _, _ -> finish() }
            .setPositiveButton("Allow") { _, _ ->
                try { startActivity(Intent(Settings.ACTION_USAGE_ACCESS_SETTINGS)) } catch (_: Exception) { startActivity(Intent(Settings.ACTION_SETTINGS)) }
            }
            .show()
    }

    private val refresh = object : Runnable {
        override fun run() {
            if (stopped) return
            worker.execute {
                val out = try { render() } catch (e: Exception) { "Activity unavailable: $e" }
                ui.post { if (!stopped) { text.text = out; ui.postDelayed(this, 3000) } }
            }
        }
    }

    private fun nameOf(uid: Int): String {
        label[uid]?.let { return it }
        val pm = packageManager
        val name = when {
            uid == android.os.Process.myUid() -> "Sailnet itself (ledger, relays)"
            uid == 1000 -> "Android system"
            uid == NetworkStats.Bucket.UID_TETHERING -> "Tethering"
            uid == NetworkStats.Bucket.UID_REMOVED -> "Removed apps"
            uid < 10000 -> "Android (uid $uid)"
            else -> {
                val pkgs = pm.getPackagesForUid(uid)
                if (pkgs.isNullOrEmpty()) "App uid $uid"
                else try { pm.getApplicationLabel(pm.getApplicationInfo(pkgs[0], 0)).toString() } catch (_: PackageManager.NameNotFoundException) { pkgs[0] }
            }
        }
        label[uid] = name
        return name
    }

    private fun human(b: Long): String = when {
        b >= 1_000_000_000 -> "%.2f GB".format(b / 1e9)
        b >= 1_000_000 -> "%.1f MB".format(b / 1e6)
        b >= 1_000 -> "%.0f kB".format(b / 1e3)
        else -> "$b B"
    }

    private fun render(): String {
        val since = Baseline.since()
        val nsm = getSystemService(Context.NETWORK_STATS_SERVICE) as NetworkStatsManager
        val rx = HashMap<Int, Long>(); val tx = HashMap<Int, Long>()
        val now = System.currentTimeMillis()
        for (type in intArrayOf(ConnectivityManager.TYPE_WIFI, ConnectivityManager.TYPE_MOBILE)) {
            val stats = try { nsm.querySummary(type, null, since, now) } catch (_: Exception) { null } ?: continue
            val b = NetworkStats.Bucket()
            while (stats.hasNextBucket()) {
                stats.getNextBucket(b)
                rx[b.uid] = (rx[b.uid] ?: 0) + b.rxBytes
                tx[b.uid] = (tx[b.uid] ?: 0) + b.txBytes
            }
            stats.close()
        }
        val uids = (rx.keys + tx.keys).filter { ((rx[it] ?: 0) + (tx[it] ?: 0)) > 0 }
            .sortedByDescending { (rx[it] ?: 0) + (tx[it] ?: 0) }
        val flows = try { JSONArray(Mobile.flows()).length() } catch (_: Exception) { 0 }
        val sb = StringBuilder()
        sb.append("Since the tunnel came up (").append(java.text.DateFormat.getTimeInstance(java.text.DateFormat.SHORT).format(java.util.Date(since))).append("), per app.\n")
        sb.append(flows).append(" flows through the tunnel in the last 10 minutes.\n\n")
        if (uids.isEmpty()) sb.append("No app traffic recorded yet. Android updates these counters every few minutes.")
        for (uid in uids) {
            sb.append(nameOf(uid)).append('\n')
            sb.append("   ↑ ").append(human(tx[uid] ?: 0)).append("   ↓ ").append(human(rx[uid] ?: 0)).append("\n\n")
        }
        return sb.toString()
    }

    override fun onDestroy() { stopped = true; ui.removeCallbacksAndMessages(null); worker.shutdown(); super.onDestroy() }

    /** The moment the tunnel came up, kept for the life of the process. */
    object Baseline {
        @Volatile private var at = 0L
        @Volatile private var seenRunning = false
        fun since(): Long {
            val running = SailVpnService.running
            if (at == 0L || (running && !seenRunning)) at = System.currentTimeMillis() - 60_000 // counters lag a little
            seenRunning = running
            return at
        }
    }
}
