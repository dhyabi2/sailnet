package net.sailnet.app

import android.content.Context
import androidx.preference.PreferenceManager
import org.json.JSONObject

/** User settings, stored with the preference framework and turned into the JSON the Go client reads. */
object Prefs {
    fun optionsJson(ctx: Context): String {
        val p = PreferenceManager.getDefaultSharedPreferences(ctx)
        val builtIn = ctx.resources.openRawResource(R.raw.bridges).bufferedReader().readText()
        val extra = p.getString("bridges", "") ?: ""
        val bridges = builtIn + "\n" + extra
        return JSONObject()
            .put("hops", 3)
            .put("excludeCC", (p.getStringSet("exclude_cc", emptySet()) ?: emptySet()).joinToString(","))
            .put("anchor", "0.0005")
            .put("maxRate", p.getString("max_rate", "0") ?: "0")
            .put("rpcUrl", rpcUrl(ctx))
            .put("rpcKey", p.getString("rpc_key", "") ?: "")
            .put("stealth", true)
            .put("bridges", bridges)
            .put("dnsUpstream", "1.1.1.1:53")
            .put("nick", nick(ctx))
            .put("censored", true)
            .toString()
    }

    fun nick(ctx: Context): String = PreferenceManager.getDefaultSharedPreferences(ctx).getString("nick", "") ?: ""
    fun setNick(ctx: Context, n: String) = PreferenceManager.getDefaultSharedPreferences(ctx).edit().putString("nick", n.trim()).apply()

    /** The endpoint to ask first. Empty or the old rpc.nano.to default means Sailnet's own. */
    fun rpcUrl(ctx: Context): String {
        val p = PreferenceManager.getDefaultSharedPreferences(ctx)
        val u = (p.getString("rpc_url", "") ?: "").trim()
        if (u.isEmpty()) return "https://www.sailnet.space/node/api"
        // An earlier build stored rpc.nano.to as its default; without a key that now means Sailnet's endpoint.
        if (u.trimEnd('/') == "https://rpc.nano.to" && (p.getString("rpc_key", "") ?: "").isBlank()) {
            p.edit().putString("rpc_url", "https://www.sailnet.space/node/api").apply()
            return "https://www.sailnet.space/node/api"
        }
        return u
    }
    fun setRpc(ctx: Context, url: String, key: String) =
        PreferenceManager.getDefaultSharedPreferences(ctx).edit().putString("rpc_url", url.trim()).putString("rpc_key", key.trim()).apply()

    fun activityConsent(ctx: Context) = PreferenceManager.getDefaultSharedPreferences(ctx).getBoolean("activity_consent", false)
    fun setActivityConsent(ctx: Context) = PreferenceManager.getDefaultSharedPreferences(ctx).edit().putBoolean("activity_consent", true).apply()

    fun autoConnect(ctx: Context) = PreferenceManager.getDefaultSharedPreferences(ctx).getBoolean("auto_connect", false)
}
