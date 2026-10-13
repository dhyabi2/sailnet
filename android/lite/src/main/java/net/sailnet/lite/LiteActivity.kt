package net.sailnet.lite

import android.content.Intent
import android.net.VpnService
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.InputType
import android.view.Gravity
import android.view.ViewGroup.LayoutParams.MATCH_PARENT
import android.view.ViewGroup.LayoutParams.WRAP_CONTENT
import android.widget.Button
import android.widget.EditText
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.TextView
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import net.sailnet.mobile.Mobile
import org.json.JSONObject
import java.text.DateFormat
import java.util.Date
import java.util.concurrent.Executors

/** The whole app: an access code and one button. */
class LiteActivity : AppCompatActivity() {
    private lateinit var code: EditText
    private lateinit var button: Button
    private lateinit var status: TextView
    private val main = Handler(Looper.getMainLooper())
    private val worker = Executors.newSingleThreadExecutor() // the gate and the client's status never run on the main thread
    @Volatile private var asking = false // waiting for the gate's answer
    private var note = "" // the last thing the gate said no to

    private val vpnPermission = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) {
        if (it.resultCode == RESULT_OK) startTunnel() else { LiteVpnService.pendingCode = ""; note = "Android needs your permission to set up the VPN." }
    }

    private val tick = object : Runnable {
        override fun run() {
            worker.execute {
                val path = try { JSONObject(Mobile.status()).optString("path") } catch (_: Exception) { "" }
                main.post { render(path.isNotEmpty()) }
            }
            main.postDelayed(this, 1000)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val pad = (24 * resources.displayMetrics.density).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER
            setPadding(pad, pad, pad, pad)
        }
        root.addView(ImageView(this).apply { setImageResource(R.drawable.ic_sail) }, LinearLayout.LayoutParams(pad * 3, pad * 3))
        root.addView(TextView(this).apply { text = getString(R.string.app_name); textSize = 22f; gravity = Gravity.CENTER; setPadding(0, pad / 2, 0, pad) }, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT))
        code = EditText(this).apply {
            hint = "Access code"
            gravity = Gravity.CENTER
            isSingleLine = true
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_CAP_CHARACTERS or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            setText(Gate.token(this@LiteActivity))
        }
        root.addView(code, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT))
        button = Button(this).apply { text = "Connect"; setOnClickListener { tapped() } }
        root.addView(button, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT).apply { topMargin = pad / 2 })
        status = TextView(this).apply { gravity = Gravity.CENTER; setPadding(0, pad, 0, 0) }
        root.addView(status, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT))
        setContentView(root)
    }

    override fun onResume() { super.onResume(); main.post(tick) }
    override fun onPause() { main.removeCallbacks(tick); super.onPause() }

    private fun render(connected: Boolean) {
        val up = LiteVpnService.running || LiteVpnService.starting
        button.isEnabled = !asking
        button.text = if (up) "Disconnect" else "Connect"
        code.isEnabled = !up && !asking
        status.text = when {
            asking -> "Checking your code…"
            LiteVpnService.running && connected -> "Connected\nYour code is good until " + DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(Gate.expires(this) * 1000))
            up -> "Connecting…"
            LiteVpnService.lastError.isNotEmpty() -> LiteVpnService.lastError
            note.isNotEmpty() -> note
            else -> "Not connected"
        }
    }

    private fun tapped() {
        if (LiteVpnService.running || LiteVpnService.starting) {
            startService(Intent(this, LiteVpnService::class.java).setAction(LiteVpnService.ACTION_STOP))
            return
        }
        val token = code.text.toString().trim()
        if (token.isEmpty()) { note = "Enter your access code first."; return }
        asking = true
        note = ""
        LiteVpnService.lastError = ""
        render(false)
        // The gate is asked before the tunnel is up: once it is, nothing leaves
        // the phone until the circuit is built.
        worker.execute {
            val needCode = !Gate.paired(this) || token != Gate.token(this)
            val r = Gate.post("join", JSONObject().put("token", token).put("wallet", Mobile.address(filesDir.absolutePath)).put("needCode", needCode))
            main.post {
                asking = false
                if (!r.ok) { note = r.error; return@post }
                if (needCode) Gate.setPaired(this, false)
                Gate.remember(this, token, r.json.optString("account"), r.json.optLong("expires"))
                LiteVpnService.pendingCode = r.json.optString("code")
                val ask = VpnService.prepare(this)
                if (ask != null) vpnPermission.launch(ask) else startTunnel()
            }
        }
    }

    private fun startTunnel() {
        val i = Intent(this, LiteVpnService::class.java)
        if (android.os.Build.VERSION.SDK_INT >= 26) startForegroundService(i) else startService(i)
    }
}
