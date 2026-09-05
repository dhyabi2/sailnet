package net.sailnet.app

import android.content.ClipData
import android.content.ClipboardManager
import android.os.Bundle
import android.widget.EditText
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import androidx.preference.PreferenceFragmentCompat
import net.sailnet.mobile.Mobile
import org.json.JSONObject

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
            findPreference<androidx.preference.Preference>("wallet_backup")?.setOnPreferenceClickListener { showBackup(); true }
            findPreference<androidx.preference.Preference>("wallet_restore")?.setOnPreferenceClickListener { showRestore(); true }
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

        // A wallet is one seed, and this phone is the only place it exists.
        // Android deletes an app's files when it is uninstalled, so without
        // these two screens a reinstall would silently cost somebody their
        // balance with no way to get it back.
        private fun showBackup() {
            val ctx = requireContext()
            val res = try {
                JSONObject(Mobile.exportWallet(ctx.filesDir.absolutePath))
            } catch (e: Exception) {
                JSONObject().put("ok", false).put("error", "could not read the wallet")
            }
            if (!res.optBoolean("ok")) {
                Toast.makeText(ctx, res.optString("error", "no wallet yet"), Toast.LENGTH_LONG).show()
                return
            }
            val seed = res.optString("seed")
            val addr = res.optString("address")
            val body = TextView(ctx).apply {
                setPadding(48, 32, 48, 8)
                setTextIsSelectable(true)
                text = "Anyone who has this seed can spend your balance, and nobody " +
                    "can recover it for you if you lose it. Write it down and keep it " +
                    "off this phone.\n\n$addr\n\n$seed"
            }
            AlertDialog.Builder(ctx)
                .setTitle("Back up your wallet")
                .setView(ScrollView(ctx).apply { addView(body) })
                .setPositiveButton("Copy seed") { _, _ ->
                    ctx.getSystemService(ClipboardManager::class.java)
                        .setPrimaryClip(ClipData.newPlainText("sailnet seed", seed))
                    Toast.makeText(ctx, "Seed copied. Paste it somewhere safe, then clear the clipboard.", Toast.LENGTH_LONG).show()
                }
                .setNegativeButton("Done", null)
                .show()
        }

        private fun showRestore() {
            val ctx = requireContext()
            if (SailVpnService.running || SailVpnService.starting) {
                Toast.makeText(ctx, "Disconnect first: the open circuits are paid for from the wallet you are replacing.", Toast.LENGTH_LONG).show()
                return
            }
            val input = EditText(ctx).apply {
                hint = "Paste the 64-character seed"
                setPadding(48, 24, 48, 24)
                isSingleLine = false
                minLines = 3
            }
            AlertDialog.Builder(ctx)
                .setTitle("Restore a wallet")
                .setMessage("This replaces the wallet on this phone. The one it replaces is kept beside it, not deleted.")
                .setView(input)
                .setPositiveButton("Restore") { _, _ ->
                    val res = try {
                        JSONObject(Mobile.importWallet(ctx.filesDir.absolutePath, input.text.toString()))
                    } catch (e: Exception) {
                        JSONObject().put("ok", false).put("error", "could not restore that backup")
                    }
                    if (res.optBoolean("ok")) {
                        AlertDialog.Builder(ctx)
                            .setTitle("Wallet restored")
                            .setMessage(res.optString("address") + "\n\nClose Sailnet and open it again to use it.")
                            .setPositiveButton("OK", null)
                            .show()
                    } else {
                        Toast.makeText(ctx, res.optString("error", "could not restore that backup"), Toast.LENGTH_LONG).show()
                    }
                }
                .setNegativeButton("Cancel", null)
                .show()
        }

}
