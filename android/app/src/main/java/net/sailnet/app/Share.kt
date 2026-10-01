package net.sailnet.app

import android.annotation.SuppressLint
import android.content.Context
import android.graphics.Bitmap
import android.graphics.Color
import android.net.ConnectivityManager
import android.net.wifi.WifiManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import androidx.preference.PreferenceManager
import com.google.zxing.BarcodeFormat
import com.google.zxing.qrcode.QRCodeWriter
import net.sailnet.mobile.Mobile
import java.net.Inet4Address
import java.net.NetworkInterface

/**
 * Sharing the circuit with other devices.
 *
 * Android's hotspot never goes through a VPN app, so guests come in through
 * a proxy instead: the Go client listens on the hotspot's own address (and
 * only there: never on the phone's Wi-Fi or mobile address, where a guest
 * would be anyone on that network, paying from this wallet).
 *
 *  - "sailnet": the app opens a local-only hotspot. It has no internet of
 *    its own, so a guest without the proxy set reaches nothing — no leak.
 *  - "mine": the user's own Android hotspot. Fixed name and password, but a
 *    guest without the proxy goes out directly, unprotected.
 *
 * Main thread only.
 */
object Share {
    const val PORT = 8080

    @Volatile var ssid = ""
    @Volatile var pass = ""
    @Volatile var addrs = "" // where guests are served, comma separated
    @Volatile var error = ""

    private val main = Handler(Looper.getMainLooper())
    private var app: Context? = null
    private var reservation: WifiManager.LocalOnlyHotspotReservation? = null
    private var hotspotStarting = false

    fun mode(ctx: Context): String =
        PreferenceManager.getDefaultSharedPreferences(ctx).getString("share", "off") ?: "off"

    /** Brings sharing in line with the setting and the tunnel. */
    fun apply(ctx: Context) {
        app = ctx.applicationContext
        val m = mode(ctx)
        if (m != "sailnet") stopHotspot()
        if (m == "off" || !SailVpnService.running) {
            stop()
            return
        }
        if (m == "sailnet") startHotspot(ctx.applicationContext)
        main.removeCallbacks(poll)
        main.post(poll)
    }

    fun stop() {
        main.removeCallbacks(poll)
        stopHotspot()
        addrs = ""
        Thread { try { Mobile.shareListen("", PORT.toLong()) } catch (_: Exception) {} }.start()
    }

    // The hotspot's address appears some seconds after it starts, and the
    // user's own hotspot can be switched on and off at any time: look again
    // every few seconds while sharing is on.
    private val poll = object : Runnable {
        override fun run() {
            val ctx = app ?: return
            Thread {
                val ips = try { hotspotAddresses(ctx) } catch (_: Exception) { emptyList() }
                val on = try { Mobile.shareListen(ips.joinToString(","), PORT.toLong()) } catch (_: Exception) { "" }
                main.post { addrs = on }
            }.start()
            main.postDelayed(this, 5000)
        }
    }

    /**
     * The IPv4 addresses of hotspot interfaces: up, private, and not carrying
     * any network this phone itself uses (its Wi-Fi, mobile data, the VPN).
     * USB and Bluetooth tethering qualify too.
     */
    private fun hotspotAddresses(ctx: Context): List<String> {
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        @Suppress("DEPRECATION")
        val own = cm.allNetworks.mapNotNull { cm.getLinkProperties(it)?.interfaceName }.toSet()
        if (own.isEmpty()) return emptyList() // cannot tell ours from a hotspot: serve nobody
        return NetworkInterface.getNetworkInterfaces().toList()
            .filter { it.isUp && !it.isLoopback && it.name !in own && !it.name.startsWith("tun") && !it.name.startsWith("dummy") }
            .flatMap { it.inetAddresses.toList() }
            .filterIsInstance<Inet4Address>()
            .filter { it.isSiteLocalAddress }
            .mapNotNull { it.hostAddress }
    }

    @SuppressLint("MissingPermission") // asked for in the share window before this mode can be chosen
    private fun startHotspot(ctx: Context) {
        if (reservation != null || hotspotStarting) return
        if (Build.VERSION.SDK_INT < 26) {
            error = "Needs Android 8"
            return
        }
        hotspotStarting = true
        error = ""
        val wm = ctx.getSystemService(WifiManager::class.java)
        try {
            wm.startLocalOnlyHotspot(object : WifiManager.LocalOnlyHotspotCallback() {
                override fun onStarted(r: WifiManager.LocalOnlyHotspotReservation) {
                    hotspotStarting = false
                    if (mode(ctx) != "sailnet" || !SailVpnService.running) {
                        r.close()
                        return
                    }
                    reservation = r
                    if (Build.VERSION.SDK_INT >= 30) {
                        val c = r.softApConfiguration
                        @Suppress("DEPRECATION")
                        ssid = (if (Build.VERSION.SDK_INT >= 33) c.wifiSsid?.toString() else c.ssid)?.trim('"') ?: ""
                        pass = c.passphrase ?: ""
                    } else {
                        @Suppress("DEPRECATION")
                        val c = r.wifiConfiguration
                        @Suppress("DEPRECATION")
                        ssid = c?.SSID?.trim('"') ?: ""
                        @Suppress("DEPRECATION")
                        pass = c?.preSharedKey?.trim('"') ?: ""
                    }
                }

                override fun onStopped() {
                    hotspotStarting = false
                    reservation = null
                    ssid = ""; pass = ""
                }

                override fun onFailed(reason: Int) {
                    hotspotStarting = false
                    error = when (reason) {
                        ERROR_INCOMPATIBLE_MODE -> "Turn off Android's hotspot first"
                        ERROR_TETHERING_DISALLOWED -> "This phone does not allow it"
                        ERROR_NO_CHANNEL -> "No free Wi-Fi channel"
                        else -> "Hotspot could not start"
                    }
                }
            }, main)
        } catch (e: SecurityException) {
            hotspotStarting = false
            error = if (Build.VERSION.SDK_INT >= 33) "Allow Nearby devices" else "Allow Location and turn it on"
        } catch (e: Exception) {
            hotspotStarting = false
            error = "Hotspot could not start"
        }
    }

    private fun stopHotspot() {
        try { reservation?.close() } catch (_: Exception) {}
        reservation = null
        ssid = ""; pass = ""
    }

    /** The standard Wi-Fi QR: a phone camera joins the network from it. */
    fun wifiQr(): String {
        fun esc(s: String) = s.replace(Regex("""([\\;,:"])"""), """\\$1""")
        return "WIFI:T:WPA;S:${esc(ssid)};P:${esc(pass)};;"
    }

    fun qr(text: String, size: Int): Bitmap {
        val m = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, size, size)
        val bmp = Bitmap.createBitmap(size, size, Bitmap.Config.RGB_565)
        for (x in 0 until size) for (y in 0 until size) bmp.setPixel(x, y, if (m.get(x, y)) Color.BLACK else Color.WHITE)
        return bmp
    }
}
