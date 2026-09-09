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
            findPreference<androidx.preference.Preference>("relay")?.setOnPreferenceClickListener { showRelayWindow(); true }
            refreshRelaySummary()
            androidx.preference.PreferenceManager.getDefaultSharedPreferences(requireContext()).registerOnSharedPreferenceChangeListener(prefWatcher)
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


        // A wallet is one seed, and this phone is the only place it exists.
        // Android deletes an app's files when it is uninstalled, so without
        // these two screens a reinstall would silently cost somebody their
        // balance with no way to get it back.
        // One window for the whole feature: which network, what is paired, add one.
        private fun showRelayWindow() {
            val ctx = requireContext()
            val p = androidx.preference.PreferenceManager.getDefaultSharedPreferences(ctx)
            val dp = { v: Int -> (v * resources.displayMetrics.density).toInt() }
            val root = android.widget.LinearLayout(ctx).apply { orientation = android.widget.LinearLayout.VERTICAL; setPadding(dp(20), dp(8), dp(20), 0) }

            // Network
            val modes = listOf("direct" to "Direct — my relay, 1 hop, free", "mine" to "My relays — free exit, paid entry", "open" to "Open network — 3 strangers")
            val group = android.widget.RadioGroup(ctx)
            val current = p.getString("mode", "mine") ?: "mine"
            modes.forEachIndexed { i, (v, label) ->
                group.addView(android.widget.RadioButton(ctx).apply { id = 100 + i; text = label; isChecked = v == current })
            }
            group.setOnCheckedChangeListener { _, id -> p.edit().putString("mode", modes[id - 100].first).apply(); refreshRelaySummary() }
            root.addView(group)

            // Paired
            val pairedView = android.widget.TextView(ctx).apply { setPadding(0, dp(12), 0, dp(4)) }
            val forget = android.widget.Button(ctx, null, android.R.attr.borderlessButtonStyle).apply { text = "Forget" }
            fun showPaired() {
                val list = pairedAccounts()
                pairedView.text = if (list.isEmpty()) "Paired: none" else "Paired: " + list.joinToString(", ") { it.take(14) + "…" }
                forget.visibility = if (list.isEmpty()) android.view.View.GONE else android.view.View.VISIBLE
            }
            forget.setOnClickListener { p.edit().putString("mine", "").apply(); showPaired(); refreshRelaySummary() }
            showPaired()
            root.addView(android.widget.LinearLayout(ctx).apply { orientation = android.widget.LinearLayout.HORIZONTAL; addView(pairedView, android.widget.LinearLayout.LayoutParams(0, android.view.ViewGroup.LayoutParams.WRAP_CONTENT, 1f)); addView(forget) })

            // Add: pick a relay, type the code from `sailnode pair`
            val relays = try { org.json.JSONArray(Mobile.relays()) } catch (_: Exception) { org.json.JSONArray() }
            val labels = ArrayList<String>(); val accounts = ArrayList<String>()
            for (i in 0 until relays.length()) {
                val r = relays.getJSONObject(i)
                if (!r.optBoolean("exit")) continue
                labels.add("${r.optString("cc")}  ${r.optString("account").take(14)}…"); accounts.add(r.optString("account"))
            }
            val spinner = android.widget.Spinner(ctx).apply { adapter = android.widget.ArrayAdapter(ctx, android.R.layout.simple_spinner_dropdown_item, if (labels.isEmpty()) listOf("connect first") else labels) }
            val code = EditText(ctx).apply { hint = "code"; inputType = android.text.InputType.TYPE_CLASS_NUMBER; filters = arrayOf(android.text.InputFilter.LengthFilter(6)) }
            val pair = android.widget.Button(ctx, null, android.R.attr.borderlessButtonStyle).apply { text = "Pair" }
            pair.setOnClickListener {
                if (accounts.isEmpty()) return@setOnClickListener
                val account = accounts[spinner.selectedItemPosition]; val typed = code.text.toString().trim()
                pair.isEnabled = false; pair.text = "…"
                Thread {
                    val res = try { org.json.JSONObject(Mobile.pairRelay(account, typed)) } catch (e: Exception) { org.json.JSONObject().put("ok", false).put("error", e.message ?: "failed") }
                    activity?.runOnUiThread {
                        pair.isEnabled = true; pair.text = "Pair"
                        if (res.optBoolean("ok")) {
                            val have = pairedAccounts().toMutableList(); if (!have.contains(account)) have.add(account)
                            p.edit().putString("mine", have.joinToString("\n")).apply(); code.setText(""); showPaired(); refreshRelaySummary()
                        } else android.widget.Toast.makeText(ctx, res.optString("error"), android.widget.Toast.LENGTH_LONG).show()
                    }
                }.start()
            }
            root.addView(android.widget.LinearLayout(ctx).apply {
                orientation = android.widget.LinearLayout.HORIZONTAL
                addView(spinner, android.widget.LinearLayout.LayoutParams(0, android.view.ViewGroup.LayoutParams.WRAP_CONTENT, 1f))
                addView(code, android.widget.LinearLayout.LayoutParams(dp(80), android.view.ViewGroup.LayoutParams.WRAP_CONTENT))
                addView(pair)
            })

            AlertDialog.Builder(ctx).setTitle("My relay").setView(root).setPositiveButton("Done", null).show()
        }

        private fun pairedAccounts(): List<String> {
            val p = androidx.preference.PreferenceManager.getDefaultSharedPreferences(requireContext())
            return (p.getString("mine", "") ?: "").lines().map { it.trim() }.filter { it.startsWith("nano_") }
        }

        private fun refreshRelaySummary() {
            val p = androidx.preference.PreferenceManager.getDefaultSharedPreferences(requireContext())
            val mode = when (p.getString("mode", "mine")) { "direct" -> "Direct"; "open" -> "Open network"; else -> "My relays" }
            val n = pairedAccounts().size
            findPreference<androidx.preference.Preference>("relay")?.summary = if (n == 0) mode else "$mode · $n paired"
        }

        private val prefWatcher = android.content.SharedPreferences.OnSharedPreferenceChangeListener { _, key ->
            if (key == "mode" || key == "mine") { refreshRelaySummary(); if (SailVpnService.running) reconnectToApply() }
        }

        private fun reconnectToApply() {
            val ctx = requireContext().applicationContext
            android.widget.Toast.makeText(ctx, "Reconnecting…", android.widget.Toast.LENGTH_SHORT).show()
            ctx.startService(android.content.Intent(ctx, SailVpnService::class.java).setAction(SailVpnService.ACTION_STOP))
            android.os.Handler(android.os.Looper.getMainLooper()).postDelayed({
                val i = android.content.Intent(ctx, SailVpnService::class.java)
                if (android.os.Build.VERSION.SDK_INT >= 26) ctx.startForegroundService(i) else ctx.startService(i)
            }, 1500)
        }

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
}
