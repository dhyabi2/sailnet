package net.sailnet.app

import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import net.sailnet.mobile.Mobile
import org.json.JSONArray

/** Every relay the client knows: where it is, how fast it answers, how it has behaved. */
class RelaysActivity : AppCompatActivity() {
    private val ui = Handler(Looper.getMainLooper())
    private lateinit var text: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_text)
        text = findViewById(R.id.text)
        ui.post(refresh)
    }

    private val refresh = object : Runnable {
        override fun run() {
            val sb = StringBuilder()
            try {
                val arr = JSONArray(Mobile.relays())
                if (arr.length() == 0) sb.append("No relays known yet. Connect first.")
                for (i in 0 until arr.length()) {
                    val r = arr.getJSONObject(i)
                    val kind = buildList {
                        if (r.optBoolean("bridge")) add("bridge")
                        if (r.optBoolean("exit")) add("exit")
                        if (r.optBoolean("home")) add("home")
                    }.joinToString(" ")
                    val rtt = if (r.has("rttMs")) "${r.getLong("rttMs")} ms" else "—"
                    sb.append("${r.optString("cc")}  ${r.optString("addr")}  $kind\n")
                    sb.append("   rtt $rtt   score %.2f   AS%d\n".format(r.optDouble("score", 0.0), r.optInt("asn")))
                    sb.append("   ${r.optString("account").take(24)}…\n\n")
                }
            } catch (e: Exception) { sb.append(e.message) }
            text.text = sb.toString()
            ui.postDelayed(this, 3000)
        }
    }

    override fun onDestroy() { ui.removeCallbacksAndMessages(null); super.onDestroy() }
}
