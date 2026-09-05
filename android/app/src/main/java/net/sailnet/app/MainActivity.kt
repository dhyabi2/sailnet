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
        findViewById<Button>(R.id.refresh).setOnClickListener {
            val b = it as Button
            b.isEnabled = false; b.text = "Checking…"
            Thread {
                val bal = try { Mobile.refresh() } catch (_: Exception) { "" }
                ui.post {
                    b.isEnabled = true; b.text = "Refresh"
                    Toast.makeText(this, if (bal.isNotEmpty()) "Balance: $bal XNO" else "Could not reach the ledger yet", Toast.LENGTH_SHORT).show()
                }
            }.start()
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
        findViewById<Button>(R.id.log).setOnClickListener { startActivity(Intent(this, LogActivity::class.java)) }
        findViewById<Button>(R.id.settings).setOnClickListener { startActivity(Intent(this, SettingsActivity::class.java)) }
        toggle.setOnClickListener {
            when {
                SailVpnService.running || SailVpnService.starting -> stopVpn()
                else -> prepareAndStart()
            }
        }
        ui.post(refresh)
        title = "Sailnet · " + Prefs.nick(this)
        checkFunds(andConnect = true)
    }



    override fun onResume() {
        super.onResume()
        if (!funded && !checkingFunds && !SailVpnService.running) checkFunds(andConnect = false)
    }

    private val refresh = object : Runnable {
        override fun run() {
            try {
                // A status call must never stall the screen; if it fails the
                // service flags still drive the texts.
                val s = try { JSONObject(Mobile.status()) } catch (_: Exception) { JSONObject() }
                val running = s.optBoolean("running")
                val starting = SailVpnService.starting || s.optBoolean("starting")
                val stage = s.optString("stage")
                val p = s.optString("path")
                val bal = s.optString("balance")
                val needsFunds = (running && p.isEmpty() && s.optBoolean("needsFunds")) ||
                    (!running && !starting && !funded && !checkingFunds)
                status.text = when {
                    running && p.isNotEmpty() -> "Connected"
                    checkingFunds && !funded -> "Checking your wallet"
                    needsFunds -> "Waiting for XNO"
                    starting || running -> if (stage.isNotEmpty() && stage != "Connected") "Connecting: $stage" else "Connecting…"
                    SailVpnService.lastError.isNotEmpty() -> "Failed"
                    else -> "Disconnected"
                }
                statusDetail.text = when {
                    running && p.isNotEmpty() -> "Exit is the last hop. All apps go through it."
                    checkingFunds && !funded -> "Reading your balance and claiming your free trial. Connect unlocks as soon as the wallet can pay."
                    needsFunds -> if (fundNote.isNotEmpty()) fundNote + " Send XNO to the address below; Sailnet connects by itself when it arrives."
                        else "Your wallet has no XNO yet. Send XNO to the address below; Sailnet connects by itself when it arrives."
                    starting || running -> if (stage.isNotEmpty()) "Step by step: relay list, relay timing, payment, then the circuit hop by hop. Nothing leaves the device until the circuit is up." else lastLogLine(s.optString("log"))
                    SailVpnService.lastError.isNotEmpty() -> "Could not connect: ${SailVpnService.lastError}. Tap Connect to try again."
                    else -> "Tap Connect to route this device through Sailnet."
                }
                path.text = p.ifEmpty { "—" }
                balance.text = if (bal.isEmpty()) "Balance unknown until first connection" else "$bal XNO"
                val up = s.optLong("bytesUp"); val down = s.optLong("bytesDown")
                traffic.text = "↑ ${human(up)}   ↓ ${human(down)}   ${s.optInt("relays")} relays"
                val low = bal.isNotEmpty() && (bal.toDoubleOrNull() ?: 0.0) < 0.0005
                fundCard.visibility = if (bal.isEmpty() || low) View.VISIBLE else View.GONE
                toggle.text = when {
                    running -> "Disconnect"
                    starting -> "Cancel"
                    checkingFunds && !funded -> "Checking wallet…"
                    !funded -> "Waiting for XNO"
                    else -> "Connect"
                }
                toggle.isEnabled = running || starting || funded
            } catch (_: Exception) {}
            ui.postDelayed(this, 1500)
        }
    }


    /**
     * The wallet comes before the button.
     *
     * A circuit is prepaid, so an empty wallet cannot connect and offering
     * Connect anyway only produces a failure the user cannot act on. On
     * opening, and again whenever the app comes back to the front, the
     * wallet is read and the free trial claimed if it is needed — none of
     * which the user has to ask for. Connect stays out of reach until the
     * answer is yes.
     */
    private fun checkFunds(andConnect: Boolean) {
        if (checkingFunds) return
        checkingFunds = true
        funded = false
        toggle.isEnabled = false
        Thread {
            val f = try {
                JSONObject(Mobile.funds(filesDir.absolutePath))
            } catch (e: Exception) {
                JSONObject().put("needsFunds", true).put("required", "0.0005")
            }
            ui.post {
                checkingFunds = false
                funded = !f.optBoolean("needsFunds", true)
                fundNote = f.optString("faucet")
                toggle.isEnabled = funded || SailVpnService.running || SailVpnService.starting
                if (funded) {
                    remindToBackUp()
                    if (andConnect && !SailVpnService.running) prepareAndStart() // opening the app means "protect me"
                } else {
                    // Keep looking: money may arrive from the faucet, or by
                    // hand from the address on screen.
                    ui.postDelayed({ checkFunds(andConnect) }, 15000)
                }
            }
        }.start()
    }


    /**
     * Ask once, early, for the one thing only the user can do.
     *
     * Uninstalling an Android app deletes everything it stored, this wallet
     * with it, and no server anywhere keeps a copy of the seed. The moment
     * to say so is when the wallet first has money in it, not after it is
     * gone, so this fires once — the first time the wallet can pay — and
     * never again.
     */
    private fun remindToBackUp() {
        val p = androidx.preference.PreferenceManager.getDefaultSharedPreferences(this)
        if (p.getBoolean("backup_reminded", false)) return
        p.edit().putBoolean("backup_reminded", true).apply()
        androidx.appcompat.app.AlertDialog.Builder(this)
            .setTitle("Save your wallet")
            .setMessage(
                "Your XNO lives in a seed kept only on this phone. Uninstalling " +
                "the app deletes it, and nobody can recover it for you.\n\n" +
                "Settings → Back up wallet shows the seed. Write it down before " +
                "you put real money in."
            )
            .setPositiveButton("Back up now") { _, _ -> startActivity(Intent(this, SettingsActivity::class.java)) }
            .setNegativeButton("Later", null)
            .show()
    }

    private var checkingFunds = false
    private var funded = false
    private var fundNote = ""

    private var askedFunds = false

    /** The wallet is empty: say so, and offer the faucet and the address. */
    private fun askFunds() {
        val addr = Mobile.address(filesDir.absolutePath)
        androidx.appcompat.app.AlertDialog.Builder(this)
            .setTitle("Fund your wallet")
            .setMessage("Sailnet pays relays in XNO and your wallet is empty. Get free XNO from a faucet (or an exchange) and send it to your address:\n\n$addr\n\n0.0005 XNO is enough to start. Sailnet connects by itself when the funds arrive.")
            .setPositiveButton("Get free XNO") { _, _ -> openLink("https://hub.nano.org/faucets") }
            .setNeutralButton("Copy address") { _, _ ->
                getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("nano address", addr))
                Toast.makeText(this, "Address copied", Toast.LENGTH_SHORT).show()
            }
            .setNegativeButton("Later", null)
            .show()
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
