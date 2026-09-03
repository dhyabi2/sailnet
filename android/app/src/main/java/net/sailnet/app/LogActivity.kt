package net.sailnet.app

import android.content.Intent
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.Menu
import android.view.MenuItem
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import net.sailnet.mobile.Mobile
import org.json.JSONObject

/** The client's recent log lines, refreshed live, with a Share action for bug reports. */
class LogActivity : AppCompatActivity() {
    private val ui = Handler(Looper.getMainLooper())
    private lateinit var text: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_text)
        text = findViewById(R.id.text)
        text.textSize = 11f
        ui.post(refresh)
    }

    private val refresh = object : Runnable {
        override fun run() {
            try { text.text = JSONObject(Mobile.status()).optString("log").ifEmpty { "No log yet." } } catch (_: Exception) {}
            ui.postDelayed(this, 2000)
        }
    }

    override fun onCreateOptionsMenu(menu: Menu): Boolean {
        menu.add(0, 1, 0, "Share").setShowAsAction(MenuItem.SHOW_AS_ACTION_ALWAYS)
        return true
    }

    override fun onOptionsItemSelected(item: MenuItem): Boolean {
        if (item.itemId == 1) {
            startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text.text.toString()), "Share log"))
            return true
        }
        return super.onOptionsItemSelected(item)
    }

    override fun onDestroy() { ui.removeCallbacksAndMessages(null); super.onDestroy() }
}
