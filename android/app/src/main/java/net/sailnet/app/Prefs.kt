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
            .put("maxRate", "0")
            .put("rpcUrl", "")
            .put("rpcKey", "")
            .put("stealth", true)
            .put("bridges", bridges)
            .put("dnsUpstream", "1.1.1.1:53")
            .put("nick", nick(ctx))
            .put("censored", true)
            .toString()
    }

    /** The nickname; generated on first use so nothing has to be asked. */
    fun nick(ctx: Context): String {
        val p = PreferenceManager.getDefaultSharedPreferences(ctx)
        val n = p.getString("nick", "") ?: ""
        if (n.isNotBlank()) return n
        val words = listOf("Falcon", "Heron", "Osprey", "Kestrel", "Tern", "Petrel", "Gannet", "Skua", "Puffin", "Swift", "Merlin", "Harrier")
        val gen = words[java.util.Random().nextInt(words.size)] + (100 + java.util.Random().nextInt(900))
        p.edit().putString("nick", gen).apply()
        return gen
    }
    fun setNick(ctx: Context, n: String) = PreferenceManager.getDefaultSharedPreferences(ctx).edit().putString("nick", n.trim()).apply()

    fun activityConsent(ctx: Context) = PreferenceManager.getDefaultSharedPreferences(ctx).getBoolean("activity_consent", false)
    fun setActivityConsent(ctx: Context) = PreferenceManager.getDefaultSharedPreferences(ctx).edit().putBoolean("activity_consent", true).apply()

    fun autoConnect(ctx: Context) = true // always: opening the app means "protect me"
}
