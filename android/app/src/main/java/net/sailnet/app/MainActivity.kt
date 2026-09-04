package net.sailnet.app

import android.app.Activity
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.Color
import android.net.Uri
import android.net.VpnService
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.View
import android.widget.Button
import android.widget.ImageView
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import com.google.zxing.BarcodeFormat
import com.google.zxing.qrcode.QRCodeWriter
import net.sailnet.mobile.Mobile
import org.json.JSONObject

class MainActivity : AppCompatActivity() {
    private lateinit var status: TextView
    private lateinit var statusDetail: TextView
    private lateinit var address: TextView
    private lateinit var balance: TextView
    private lateinit var path: TextView
    private lateinit var traffic: TextView
    private lateinit var toggle: Button
    private lateinit var qr: ImageView
    private lateinit var fundCard: View
    private val ui = Handler(Looper.getMainLooper())

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        status = findViewById(R.id.status)
        statusDetail = findViewById(R.id.statusDetail)
        supportActionBar?.subtitle = "v" + BuildConfig.VERSION_NAME
        address = findViewById(R.id.address)
        balance = findViewById(R.id.balance)
        path = findViewById(R.id.path)
        traffic = findViewById(R.id.traffic)
        toggle = findViewById(R.id.toggle)
        qr = findViewById(R.id.qr)
        fundCard = findViewById(R.id.fundCard)

        val addr = Mobile.address(filesDir.absolutePath)
        address.text = addr
        if (Prefs.nick(this).isNotEmpty()) title = "Sailnet · " + Prefs.nick(this)
        qr.setImageBitmap(qrBitmap("nano:$addr", 512))

        findViewById<Button>(R.id.copy).setOnClickListener {
            getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("nano address", addr))
            Toast.makeText(this, "Address copied", Toast.LENGTH_SHORT).show()
        }
        findViewById<Button>(R.id.share).setOnClickListener {
            startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, addr), "Share address"))
        }
        // Where to get XNO. Plain ACTION_VIEW intents, so the system browser
        // (or whatever handles https links) opens them.
        findViewById<Button>(R.id.faucets).setOnClickListener { openLink("https://hub.nano.org/faucets") }
        findViewById<Button>(R.id.binance).setOnClickListener { openLink("https://www.binance.com/en/trade/XNO_USDT") }
        findViewById<Button>(R.id.kraken).setOnClickListener { openLink("https://www.kraken.com/prices/nano") }
        findViewById<Button>(R.id.newExit).setOnClickListener { Mobile.rebuild(); Toast.makeText(this, "Building a new circuit", Toast.LENGTH_SHORT).show() }
        findViewById<Button>(R.id.relays).setOnClickListener { startActivity(Intent(this, RelaysActivity::class.java)) }
        findViewById<Button>(R.id.activity).setOnClickListener { startActivity(Intent(this, ActivityActivity::class.java)) }
        findViewById<Button>(R.id.log).setOnClickListener { startActivity(Intent(this, LogActivity::class.java)) }
        findViewById<Button>(R.id.settings).setOnClickListener { startActivity(Intent(this, SettingsActivity::class.java)) }
        toggle.setOnClickListener { if (SailVpnService.running) stopVpn() else prepareAndStart() }
        ui.post(refresh)
        when {
            Prefs.nick(this).isEmpty() -> askNickname()
            Prefs.autoConnect(this) && !SailVpnService.running -> prepareAndStart()
        }
    }

    /** First launch: a nickname that replaces the wallet address and device IPs in every log and screen. */
    private fun askNickname() {
        val input = android.widget.EditText(this)
        input.hint = "e.g. Falcon"
        input.setSingleLine()
        androidx.appcompat.app.AlertDialog.Builder(this)
            .setTitle("Pick a nickname")
            .setMessage("Sailnet never shows your wallet address or your device's address in logs, traces or screens. It shows this nickname instead. It is stored only on this phone.")
            .setView(input)
            .setCancelable(false)
            .setPositiveButton("Save") { _, _ ->
                val n = input.text.toString().trim().ifEmpty { "Sailor" }
                Prefs.setNick(this, n)
                title = "Sailnet · $n"
            }
            .show()
    }


    private val refresh = object : Runnable {
        override fun run() {
            try {
                val s = JSONObject(Mobile.status())
                val running = s.optBoolean("running")
                val p = s.optString("path")
                val bal = s.optString("balance")
                status.text = when {
                    SailVpnService.blackhole -> "Blocked (kill switch)"
                    running && p.isNotEmpty() -> "Connected"
                    running -> "Building circuit…"
                    SailVpnService.lastError.isNotEmpty() -> "Failed"
                    else -> "Disconnected"
                }
                statusDetail.text = when {
                    running && p.isNotEmpty() -> "Exit is the last hop. All apps go through it."
                    running -> lastLogLine(s.optString("log"))
                    SailVpnService.lastError.isNotEmpty() -> SailVpnService.lastError
                    else -> "Tap Connect to route this device through Sailnet."
                }
                path.text = p.ifEmpty { "—" }
                balance.text = if (bal.isEmpty()) "Balance unknown until first connection" else "$bal XNO"
                val up = s.optLong("bytesUp"); val down = s.optLong("bytesDown")
                traffic.text = "↑ ${human(up)}   ↓ ${human(down)}   ${s.optInt("relays")} relays"
                val low = bal.isNotEmpty() && (bal.toDoubleOrNull() ?: 0.0) < 0.0005
                fundCard.visibility = if (bal.isEmpty() || low) View.VISIBLE else View.GONE
                toggle.text = if (running) "Disconnect" else "Connect"
            } catch (_: Exception) {}
            ui.postDelayed(this, 1500)
        }
    }

    private fun lastLogLine(log: String): String {
        val line = log.trim().lines().lastOrNull() ?: return "Starting…"
        return line.substringAfter(" ", line).substringAfter(" ", line)
    }

    private fun human(b: Long): String = when {
        b > 1_000_000_000 -> "%.2f GB".format(b / 1e9)
        b > 1_000_000 -> "%.1f MB".format(b / 1e6)
        b > 1000 -> "%.0f kB".format(b / 1e3)
        else -> "$b B"
    }

    private fun openLink(url: String) {
        try {
            startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        } catch (e: Exception) {
            getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("link", url))
            Toast.makeText(this, "No browser found; link copied", Toast.LENGTH_LONG).show()
        }
    }

    private fun qrBitmap(text: String, size: Int): Bitmap {
        val m = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, size, size)
        val bmp = Bitmap.createBitmap(size, size, Bitmap.Config.RGB_565)
        for (x in 0 until size) for (y in 0 until size) bmp.setPixel(x, y, if (m.get(x, y)) Color.BLACK else Color.WHITE)
        return bmp
    }

    private fun prepareAndStart() {
        val intent = VpnService.prepare(this)
        if (intent != null) startActivityForResult(intent, 1) else startVpn()
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == 1 && resultCode == Activity.RESULT_OK) startVpn()
    }

    private fun startVpn() {
        SailVpnService.lastError = ""
        val i = Intent(this, SailVpnService::class.java)
        if (android.os.Build.VERSION.SDK_INT >= 26) startForegroundService(i) else startService(i)
    }

    private fun stopVpn() {
        startService(Intent(this, SailVpnService::class.java).setAction(SailVpnService.ACTION_STOP))
    }
}
