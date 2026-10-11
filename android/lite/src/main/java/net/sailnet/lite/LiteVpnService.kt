package net.sailnet.lite

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import net.sailnet.mobile.Mobile
import net.sailnet.mobile.Protector
import org.json.JSONObject

/**
 * The tunnel of the lite app: one hop, straight through the relay this
 * phone is paired with, nothing paid. Pairing happens here, once, with the
 * code the gate handed over just before the tunnel came up.
 */
class LiteVpnService : VpnService(), Protector {
    private var tun: ParcelFileDescriptor? = null
    @Volatile private var session = 0 // a watcher from an earlier connect stops itself

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopTunnel()
            stopSelf()
            return START_NOT_STICKY
        }
        if (tun != null || starting) return START_STICKY // one start at a time
        starting = true
        lastError = ""
        startForeground(1, notification("Connecting…"))
        val builder = Builder()
            .setSession("Sailnet Lite")
            .setMtu(MTU)
            .addAddress("10.8.0.2", 32)
            .addRoute("0.0.0.0", 0)
            .addAddress("fd5a:11::2", 128)
            .addRoute("::", 0) // IPv6 goes into the tunnel too, so nothing bypasses it
            .addDnsServer("10.8.0.1")
            .setBlocking(false)
        val pfd = builder.establish() ?: run { starting = false; stopSelf(); return START_NOT_STICKY }
        tun = pfd
        // A descriptor of its own for the Go side: it closes what it is given,
        // and so does this service (see SailVpnService in :app).
        val goFd = pfd.dup().detachFd()
        val account = Gate.account(this)
        val code = pendingCode
        pendingCode = ""
        val mine = session + 1
        session = mine
        Thread {
            var handed = false // the Go side has the descriptor and will close it
            try {
                // Not yet paired, the relay is not named as ours: a relay that
                // refuses an owner's circuit is rested for an hour.
                Mobile.start(filesDir.absolutePath, options(if (Gate.paired(this)) account else ""), goFd.toLong(), MTU.toLong(), this)
                handed = true
                if (!Gate.paired(this)) {
                    pair(account, code)
                    Gate.setPaired(this, true)
                    Mobile.rebuild()
                }
                running = true
                starting = false
                updateNotification("Connected")
                watch(mine)
            } catch (e: Exception) {
                // No black hole: a failed start tears the tunnel down and says why.
                if (!handed) try { ParcelFileDescriptor.adoptFd(goFd).close() } catch (_: Exception) {}
                fail(e.message ?: "Could not connect")
            }
        }.start()
        return START_STICKY
    }

    /** Hands the gate's code to the relay; the relay list may take a moment to arrive first. */
    private fun pair(account: String, code: String) {
        if (code.isEmpty()) throw Exception("Tap Connect again to finish setting up.")
        val deadline = System.currentTimeMillis() + 90_000
        while (true) {
            val res = JSONObject(Mobile.pairRelay(account, code))
            if (res.optBoolean("ok")) return
            val err = res.optString("error")
            if (!err.contains("not known to this client yet") || System.currentTimeMillis() > deadline) {
                throw Exception("Could not set up the connection. Tap Connect to try again.")
            }
            Thread.sleep(3000)
        }
    }

    /** While connected, ask the gate now and then whether the code still stands. */
    private fun watch(mine: Int) {
        while (session == mine && running) {
            val left = Gate.expires(this) - System.currentTimeMillis() / 1000
            if (left <= 0) return fail("This code has expired.")
            Thread.sleep(minOf(CHECK_EVERY_MS, left * 1000 + 1000))
            if (session != mine || !running) return
            val r = Gate.post("check", JSONObject().put("token", Gate.token(this)).put("wallet", Mobile.address(filesDir.absolutePath)))
            if (r.refused) return fail(r.error)
            if (r.ok) Gate.remember(this, Gate.token(this), Gate.account(this), r.json.optLong("expires"))
        }
    }

    private fun fail(message: String) {
        lastError = message
        android.os.Handler(mainLooper).post { stopTunnel(); stopSelf() }
    }

    private fun options(mine: String): String {
        val bridges = resources.openRawResource(R.raw.bridges).bufferedReader().readText()
        return JSONObject()
            .put("hops", 1)
            .put("anchor", "0.0005")
            .put("maxRate", "0")
            .put("rpcUrl", "")
            .put("rpcKey", "")
            .put("stealth", true)
            .put("bridges", bridges)
            .put("mine", mine)
            .put("mode", "direct")
            .put("dnsUpstream", "1.1.1.1:53")
            .put("nick", "Lite")
            .put("censored", true)
            .toString()
    }

    override fun protect(fd: Long): Boolean = protect(fd.toInt())

    private fun stopTunnel() {
        session++
        starting = false
        try { Mobile.stop() } catch (_: Exception) {}
        tun?.close()
        tun = null
        running = false
        stopForeground(STOP_FOREGROUND_REMOVE)
    }

    override fun onDestroy() {
        stopTunnel()
        super.onDestroy()
    }

    override fun onRevoke() {
        stopTunnel()
        stopSelf()
    }

    private fun notification(text: String): Notification {
        val nm = getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= 26) {
            nm.createNotificationChannel(NotificationChannel(CHANNEL, "Sailnet Lite", NotificationManager.IMPORTANCE_LOW))
        }
        val open = PendingIntent.getActivity(this, 0, Intent(this, LiteActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val stop = PendingIntent.getService(this, 1, Intent(this, LiteVpnService::class.java).setAction(ACTION_STOP), PendingIntent.FLAG_IMMUTABLE)
        val b = if (Build.VERSION.SDK_INT >= 26) Notification.Builder(this, CHANNEL) else Notification.Builder(this)
        b.setContentTitle("Sailnet Lite").setContentText(text).setSmallIcon(R.drawable.ic_sail).setContentIntent(open).setOngoing(true)
        b.addAction(Notification.Action.Builder(null, "Disconnect", stop).build())
        return b.build()
    }

    private fun updateNotification(text: String) {
        getSystemService(NotificationManager::class.java).notify(1, notification(text))
    }

    companion object {
        const val ACTION_STOP = "net.sailnet.lite.STOP"
        const val CHANNEL = "sailnet-lite"
        const val MTU = 1500
        private const val CHECK_EVERY_MS = 15 * 60 * 1000L
        @Volatile var running = false
        @Volatile var starting = false // between the tap and the tunnel being up
        @Volatile var lastError = ""
        /** The pairing code from the gate, for the connect that is about to start. */
        @Volatile var pendingCode = ""
    }
}
