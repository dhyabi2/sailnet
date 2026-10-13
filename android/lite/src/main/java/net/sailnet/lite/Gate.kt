package net.sailnet.lite

import android.content.Context
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/** The gate that decides who may use the lite app, and what this phone remembers of its answer. */
object Gate {
    /** status is the HTTP status, or 0 when the gate could not be reached. */
    class Reply(val status: Int, val json: JSONObject) {
        val ok get() = status == 200 && json.optBoolean("ok")
        /** The gate said no (bad, expired, switched off) — not a network failure. */
        val refused get() = status == 403
        val error: String get() = json.optString("error").ifEmpty { "Could not reach the service. Check your connection and try again." }
    }

    fun post(path: String, body: JSONObject): Reply {
        var c: HttpURLConnection? = null
        return try {
            c = URL("${BuildConfig.GATE}/api/$path").openConnection() as HttpURLConnection
            c.requestMethod = "POST"
            c.connectTimeout = 15000
            c.readTimeout = 60000 // a join waits for the relay to mint a code
            c.doOutput = true
            c.setRequestProperty("Content-Type", "application/json")
            c.outputStream.use { it.write(body.toString().toByteArray()) }
            val status = c.responseCode
            val text = (if (status < 400) c.inputStream else c.errorStream)?.bufferedReader()?.readText() ?: ""
            Reply(status, try { JSONObject(text) } catch (_: Exception) { JSONObject() })
        } catch (_: Exception) {
            Reply(0, JSONObject())
        } finally {
            c?.disconnect()
        }
    }

    private fun prefs(ctx: Context) = ctx.getSharedPreferences("lite", Context.MODE_PRIVATE)

    fun token(ctx: Context) = prefs(ctx).getString("token", "") ?: ""
    fun account(ctx: Context) = prefs(ctx).getString("account", "") ?: ""
    fun expires(ctx: Context) = prefs(ctx).getLong("expires", 0)
    /** Paired with the relay named by account(): the relay lets this wallet ride free. */
    fun paired(ctx: Context) = prefs(ctx).getBoolean("paired", false)

    fun remember(ctx: Context, token: String, account: String, expires: Long) {
        val p = prefs(ctx)
        val e = p.edit().putString("token", token).putString("account", account).putLong("expires", expires)
        if (account != p.getString("account", "")) e.putBoolean("paired", false) // a different relay: pair again
        e.apply()
    }
    fun setPaired(ctx: Context, paired: Boolean) = prefs(ctx).edit().putBoolean("paired", paired).apply()
}
