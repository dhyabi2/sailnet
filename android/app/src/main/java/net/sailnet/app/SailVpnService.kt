package net.sailnet.app

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

/** Establishes the TUN interface and hands its descriptor to the Sailnet client. */
class SailVpnService : VpnService(), Protector {
    private var tun: ParcelFileDescriptor? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopTunnel()
            stopSelf()
            return START_NOT_STICKY
        }
        if (tun != null || starting) return START_STICKY // one start at a time
        starting = true
        startForeground(1, notification("Connecting…"))
        val builder = Builder()
            .setSession("Sailnet")
            .setMtu(MTU)
            .addAddress("10.8.0.2", 32)
            .addRoute("0.0.0.0", 0)
            .addAddress("fd5a:11::2", 128)
            .addRoute("::", 0) // IPv6 goes into the tunnel too, so nothing bypasses it
            .addDnsServer("10.8.0.1")
            .setBlocking(false)
        val pfd = builder.establish() ?: run { stopSelf(); return START_NOT_STICKY }
        tun = pfd
        // Give the Go side a descriptor of its own.
        //
        // It wraps whatever number it is handed and closes it when the
        // tunnel stops; this service closes its own ParcelFileDescriptor at
        // the same moment. Handing over pfd.fd meant both were closing the
        // one descriptor, and Android kills a process that does that —
        // "fdsan: double-close of file descriptor" — so every Disconnect
        // aborted the app instead of stopping it. Two descriptors onto the
        // same tunnel, one owner each, and the kernel releases it when the
        // second one goes.
        val goFd = pfd.dup().detachFd()
        val options = Prefs.optionsJson(this)
        Thread {
            try {
                Mobile.start(filesDir.absolutePath, options, goFd.toLong(), MTU.toLong(), this)
                running = true
                starting = false
                SailTileService.refresh(this)
                updateNotification("Connected through the Sailnet circuit")
            } catch (e: Exception) {
                // No black hole: a failed start tears the tunnel down and says
                // why, so the phone keeps its normal connection and the user
                // can simply tap Connect again.
                lastError = e.message ?: "start failed"
                starting = false
                updateNotification("Sailnet could not connect: $lastError")
                try { ParcelFileDescriptor.adoptFd(goFd).close() } catch (_: Exception) {}
                stopTunnel()
                stopSelf()
            }
        }.start()
        return START_STICKY
    }

    override fun protect(fd: Long): Boolean = protect(fd.toInt())

    private fun stopTunnel() {
        starting = false
        try { Mobile.stop() } catch (_: Exception) {}
        tun?.close()
        tun = null
        running = false
        SailTileService.refresh(this)
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
            nm.createNotificationChannel(NotificationChannel(CHANNEL, "Sailnet", NotificationManager.IMPORTANCE_LOW))
        }
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val stop = PendingIntent.getService(this, 1, Intent(this, SailVpnService::class.java).setAction(ACTION_STOP), PendingIntent.FLAG_IMMUTABLE)
        val b = if (Build.VERSION.SDK_INT >= 26) Notification.Builder(this, CHANNEL) else Notification.Builder(this)
        b.setContentTitle("Sailnet").setContentText(text).setSmallIcon(R.drawable.ic_sail).setContentIntent(open).setOngoing(true)
        b.addAction(Notification.Action.Builder(null, "Disconnect", stop).build())
        return b.build()
    }

    private fun updateNotification(text: String) {
        getSystemService(NotificationManager::class.java).notify(1, notification(text))
    }

    companion object {
        const val ACTION_STOP = "net.sailnet.app.STOP"
        const val CHANNEL = "sailnet"
        const val MTU = 1500
        @Volatile var running = false
        @Volatile var starting = false // between the tap and the tunnel being up
        @Volatile var lastError = ""
    }
}
