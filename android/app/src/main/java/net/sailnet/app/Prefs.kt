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
        val bridges = if (p.getBoolean("use_builtin_bridges", true)) builtIn + "\n" + extra else extra
        return JSONObject()
            .put("hops", (p.getString("hops", "3") ?: "3").toIntOrNull() ?: 3)
            .put("exitCC", p.getString("exit_cc", "") ?: "")
            .put("anchor", p.getString("anchor", "0.0005") ?: "0.0005")
            .put("maxRate", p.getString("max_rate", "0") ?: "0")
            .put("rpcUrl", rpcUrl(ctx))
            .put("rpcKey", p.getString("rpc_key", "") ?: "")
            .put("stealth", p.getBoolean("stealth", true))
            .put("bridges", bridges)
            .put("dnsUpstream", p.getString("dns_upstream", "1.1.1.1:53") ?: "1.1.1.1:53")
            .put("nick", nick(ctx))
            .put("censored", p.getBoolean("censored", false))
            .toString()
    }

    fun nick(ctx: Context): String = PreferenceManager.getDefaultSharedPreferences(ctx).getString("nick", "") ?: ""
    fun setNick(ctx: Context, n: String) = PreferenceManager.getDefaultSharedPreferences(ctx).edit().putString("nick", n.trim()).apply()

    fun rpcUrl(ctx: Context): String = PreferenceManager.getDefaultSharedPreferences(ctx).getString("rpc_url", "") ?: ""
    fun setRpc(ctx: Context, url: String, key: String) =
        PreferenceManager.getDefaultSharedPreferences(ctx).edit().putString("rpc_url", url.trim()).putString("rpc_key", key.trim()).apply()

    fun autoConnect(ctx: Context) = PreferenceManager.getDefaultSharedPreferences(ctx).getBoolean("auto_connect", false)
}
