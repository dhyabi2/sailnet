package net.sailnet.app

import android.os.Bundle
import androidx.appcompat.app.AppCompatActivity
import androidx.preference.PreferenceFragmentCompat

class SettingsActivity : AppCompatActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        supportFragmentManager.beginTransaction().replace(android.R.id.content, Fragment()).commit()
    }

    class Fragment : PreferenceFragmentCompat() {
        override fun onCreatePreferences(savedInstanceState: Bundle?, rootKey: String?) {
            setPreferencesFromResource(R.xml.prefs, rootKey)
            // Exit exclusion: the choices are the countries of the relays this
            // client knows, so the user picks from real options, never types.
            findPreference<androidx.preference.MultiSelectListPreference>("exclude_cc")?.let { pref ->
                val codes = try { org.json.JSONArray(net.sailnet.mobile.Mobile.countries()) } catch (_: Exception) { org.json.JSONArray() }
                val values = ArrayList<String>(); val labels = ArrayList<String>()
                for (i in 0 until codes.length()) {
                    val c = codes.getString(i)
                    values.add(c); labels.add(java.util.Locale("", c).displayCountry.ifEmpty { c } + " ($c)")
                }
                if (values.isEmpty()) { pref.summary = "No relays known yet; connect once, then choose." }
                pref.entries = labels.toTypedArray(); pref.entryValues = values.toTypedArray()
                pref.summaryProvider = androidx.preference.Preference.SummaryProvider<androidx.preference.MultiSelectListPreference> { p ->
                    if (p.values.isEmpty()) "None excluded. Tap to choose countries never to exit through." else "Excluded: " + p.values.sorted().joinToString(", ")
                }
            }
        }
    }
}
