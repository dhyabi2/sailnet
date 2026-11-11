package net.sailnet.app

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.os.Bundle
import android.text.InputFilter
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.view.ViewGroup.LayoutParams.MATCH_PARENT
import android.view.ViewGroup.LayoutParams.WRAP_CONTENT
import android.widget.Button
import android.widget.CheckBox
import android.widget.EditText
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.RadioButton
import android.widget.RadioGroup
import android.widget.ScrollView
import android.widget.Spinner
import android.widget.ArrayAdapter
import android.widget.Switch
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import androidx.preference.PreferenceManager
import androidx.viewpager2.widget.ViewPager2
import net.sailnet.mobile.Mobile
import org.json.JSONArray
import org.json.JSONObject
import java.util.Locale

/**
 * Interactive Intro & Options Tour.
 *
 * Shown automatically on first launch (and any time from the "Guide" button
 * on the main screen or in Settings). Lets the user see and configure every
 * capability Sailnet offers right inside the tour:
 *   1. How Sailnet Works — 3-hop onion routing disguised as Chrome HTTPS + DNS-over-Circuit
 *   2. Network Modes & My Relay — Open network (3 hops), My relays (free exit), Direct (1 hop free VPN) + 6-digit pairing
 *   3. Exit Country Exclusion & Censorship Bridges — exclude countries, paste unlisted bridges, screen-off pause
 *   4. Share Connection (Hotspot) — share the encrypted tunnel over a zero-leak Sailnet Wi-Fi hotspot with QR code
 *   5. Self-Custody XNO Wallet — automatic free trial, pay-per-MiB meter, seed backup & restore, nickname
 */
class IntroActivity : AppCompatActivity() {

    private lateinit var pager: ViewPager2
    private lateinit var dotsBox: LinearLayout
    private lateinit var backBtn: Button
    private lateinit var nextBtn: Button
    private lateinit var skipBtn: TextView
    private val pageCount = 5

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(Color.parseColor("#06080A"))
            layoutParams = ViewGroup.LayoutParams(MATCH_PARENT, MATCH_PARENT)
        }

        // Top bar: brand badge + Skip / Close
        val topBar = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setPadding(dp(20), dp(16), dp(20), dp(8))
        }
        val logo = ImageView(this).apply {
            setImageResource(R.drawable.ic_sail)
        }
        topBar.addView(logo, LinearLayout.LayoutParams(dp(28), dp(28)))
        val brandTitle = TextView(this).apply {
            text = "SAILNET · GUIDE & OPTIONS"
            setTextColor(Color.parseColor("#14B8A6"))
            textSize = 12f
            typeface = Typeface.create(Typeface.DEFAULT, Typeface.BOLD)
            letterSpacing = 0.14f
            setPadding(dp(10), 0, 0, 0)
        }
        topBar.addView(brandTitle, LinearLayout.LayoutParams(0, WRAP_CONTENT, 1f))
        skipBtn = TextView(this).apply {
            text = "Skip"
            setTextColor(Color.parseColor("#94A3B8"))
            textSize = 14f
            setPadding(dp(12), dp(6), dp(4), dp(6))
            setOnClickListener { finishIntro() }
        }
        topBar.addView(skipBtn)
        root.addView(topBar, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT))

        // Pager
        pager = ViewPager2(this).apply {
            adapter = IntroAdapter()
            registerOnPageChangeCallback(object : ViewPager2.OnPageChangeCallback() {
                override fun onPageSelected(position: Int) {
                    updateControls(position)
                }
            })
        }
        root.addView(pager, LinearLayout.LayoutParams(MATCH_PARENT, 0, 1f))

        // Bottom bar: Back, Dots, Next / Start
        val bottomBar = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setPadding(dp(20), dp(12), dp(20), dp(20))
            setBackgroundColor(Color.parseColor("#0B0F14"))
        }
        backBtn = Button(this).apply {
            text = "Back"
            isAllCaps = false
            setTextColor(Color.parseColor("#CBD5E1"))
            background = pillBg("#1E293B", "#334155")
            setOnClickListener {
                if (pager.currentItem > 0) pager.currentItem -= 1
            }
        }
        bottomBar.addView(backBtn, LinearLayout.LayoutParams(dp(96), dp(46)))

        dotsBox = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER
        }
        bottomBar.addView(dotsBox, LinearLayout.LayoutParams(0, WRAP_CONTENT, 1f))

        nextBtn = Button(this).apply {
            text = "Next"
            isAllCaps = false
            setTextColor(Color.WHITE)
            typeface = Typeface.create(Typeface.DEFAULT, Typeface.BOLD)
            background = pillBg("#0D9488", "#14B8A6")
            setOnClickListener {
                if (pager.currentItem < pageCount - 1) {
                    pager.currentItem += 1
                } else {
                    finishIntro()
                }
            }
        }
        bottomBar.addView(nextBtn, LinearLayout.LayoutParams(dp(128), dp(46)))
        root.addView(bottomBar, LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT))

        setContentView(root)
        val startPage = intent.getIntExtra("page", 0).coerceIn(0, pageCount - 1)
        if (startPage > 0) pager.setCurrentItem(startPage, false)
        updateControls(startPage)
    }

    private fun updateControls(pos: Int) {
        backBtn.visibility = if (pos == 0) View.INVISIBLE else View.VISIBLE
        skipBtn.text = if (pos == pageCount - 1) "Close" else "Skip"
        nextBtn.text = if (pos == pageCount - 1) "Get Started" else "Next"

        dotsBox.removeAllViews()
        for (i in 0 until pageCount) {
            val active = i == pos
            val v = View(this).apply {
                background = GradientDrawable().apply {
                    shape = GradientDrawable.RECTANGLE
                    cornerRadius = dp(4).toFloat()
                    setColor(Color.parseColor(if (active) "#14B8A6" else "#334155"))
                }
            }
            val lp = LinearLayout.LayoutParams(if (active) dp(24) else dp(8), dp(8)).apply {
                marginStart = dp(4)
                marginEnd = dp(4)
            }
            dotsBox.addView(v, lp)
        }
    }

    private fun finishIntro() {
        PreferenceManager.getDefaultSharedPreferences(this)
            .edit()
            .putBoolean("intro_seen_v2", true)
            .apply()
        finish()
    }

    private inner class IntroAdapter : androidx.recyclerview.widget.RecyclerView.Adapter<PageHolder>() {
        override fun onCreateViewHolder(parent: ViewGroup, viewType: Int): PageHolder {
            val scroll = ScrollView(this@IntroActivity).apply {
                isFillViewport = true
                layoutParams = ViewGroup.LayoutParams(MATCH_PARENT, MATCH_PARENT)
            }
            return PageHolder(scroll)
        }

        override fun getItemCount(): Int = pageCount

        override fun onBindViewHolder(holder: PageHolder, position: Int) {
            holder.scroll.removeAllViews()
            val page = when (position) {
                0 -> buildOverviewPage()
                1 -> buildModesAndRelayPage()
                2 -> buildRoutingAndBridgesPage()
                3 -> buildHotspotSharePage()
                else -> buildWalletAndIdentityPage()
            }
            holder.scroll.addView(page)
        }
    }

    private class PageHolder(val scroll: ScrollView) : androidx.recyclerview.widget.RecyclerView.ViewHolder(scroll)

    // ------------------------------------------------------------------------
    // Page 1: Architecture & All Available Capabilities
    // ------------------------------------------------------------------------
    private fun buildOverviewPage(): View {
        val col = pageColumn()
        col.addView(stepBadge("1 OF 5 · ARCHITECTURE & CAPABILITIES"))
        col.addView(heroTitle("Privacy that pays its own way."))
        col.addView(heroSub("Sailnet routes every app on your phone through 3-hop ChaCha20-Poly1305 onion circuits disguised as ordinary Chrome HTTPS — paid per megabyte in XNO with no accounts and no central company."))

        col.addView(featureCard(
            "3-Hop Onion Circuit + uTLS Chrome Disguise",
            "Entry, middle, and exit relays are chosen across distinct countries and ASNs. The entry sees your IP but never your destination; the exit sees the destination but never who you are."
        ))
        col.addView(featureCard(
            "Pipelined DNS-over-Circuit",
            "Every DNS lookup travels inside the encrypted tunnel over a persistent TCP pipeline to the exit resolver. Your local SIM/Wi-Fi network sees zero DNS traffic."
        ))
        col.addView(featureCard(
            "All Options Available at a Glance",
            "• 3 Network Modes: Open Network, My Relays (free exit), or Direct (1-hop free VPN)\n" +
            "• Exit Country Exclusion: ban any country from ever acting as your exit\n" +
            "• Unlisted Bridges: built-in + custom bridge lines for censored networks\n" +
            "• Wi-Fi Hotspot Sharing: share the encrypted circuit with laptops & guests\n" +
            "• Smart Idle & Cost Meter: pauses when screen is off; tracks every MiB & XNO"
        ))
        return col
    }

    // ------------------------------------------------------------------------
    // Page 2: Network Modes & Pairing Your Own Relay
    // ------------------------------------------------------------------------
    private fun buildModesAndRelayPage(): View {
        val col = pageColumn()
        val prefs = PreferenceManager.getDefaultSharedPreferences(this)

        col.addView(stepBadge("2 OF 5 · NETWORK MODES & MY RELAY"))
        col.addView(heroTitle("Choose how your circuits are built."))
        col.addView(heroSub("Use the global relay market, or pair a relay you run (`sailnode pair`) to ride it completely free."))

        val modeCard = cardContainer()
        modeCard.addView(sectionLabel("ACTIVE NETWORK MODE"))

        val modes = listOf(
             Triple("mine", "My relays (Recommended)", "3 hops. If you paired a relay you own, it is used as your free exit or middle hop while a paid stranger acts as entry."),
             Triple("open", "Open network", "3 hops drawn from all verified public relays weighted by speed, price, and country diversity."),
             Triple("direct", "Direct (1-hop personal VPN)", "1 hop straight through your own paired relay. Fastest speed, 100% free, no XNO spent.")
        )

        val currentMode = prefs.getString("mode", "mine") ?: "mine"
        val rg = RadioGroup(this).apply {
            orientation = RadioGroup.VERTICAL
            setPadding(0, dp(6), 0, 0)
        }
        modes.forEachIndexed { idx, (key, title, desc) ->
            val rb = RadioButton(this).apply {
                id = 500 + idx
                text = "$title\n$desc"
                setTextColor(Color.parseColor("#E2E8F0"))
                textSize = 13.5f
                setPadding(dp(8), dp(10), dp(8), dp(10))
                isChecked = key == currentMode
            }
            rg.addView(rb)
        }
        rg.setOnCheckedChangeListener { _, checkedId ->
            val chosen = modes.getOrNull(checkedId - 500)?.first ?: "mine"
            prefs.edit().putString("mode", chosen).apply()
            reconnectIfRunning()
        }
        modeCard.addView(rg)
        col.addView(modeCard)

        // Pair a relay card
        val pairCard = cardContainer()
        pairCard.addView(sectionLabel("PAIR YOUR OWN RELAY (`sailnode pair`)"))
        val pairedText = TextView(this).apply {
            setTextColor(Color.parseColor("#94A3B8"))
            textSize = 13f
            setPadding(0, dp(6), 0, dp(8))
        }
        fun updatePairedLabel() {
            val list = (prefs.getString("mine", "") ?: "").lines().map { it.trim() }.filter { it.startsWith("nano_") }
            pairedText.text = if (list.isEmpty()) "No relays paired yet. Run `sailnode pair` on your relay to get a 6-digit code, or paste its nano_ address below."
            else "Paired relays (${list.size}): " + list.joinToString(", ") { it.take(16) + "…" }
        }
        updatePairedLabel()
        pairCard.addView(pairedText)

        val addBtn = actionButton("Open My Relay & Pairing Window") {
            startActivity(Intent(this, SettingsActivity::class.java))
        }
        pairCard.addView(addBtn)
        col.addView(pairCard)
        return col
    }

    // ------------------------------------------------------------------------
    // Page 3: Exit Country Exclusion, Censorship Bridges & Screen-Off Pause
    // ------------------------------------------------------------------------
    private fun buildRoutingAndBridgesPage(): View {
        val col = pageColumn()
        val prefs = PreferenceManager.getDefaultSharedPreferences(this)

        col.addView(stepBadge("3 OF 5 · EXIT COUNTRIES & CENSORSHIP BRIDGES"))
        col.addView(heroTitle("Control where your traffic exits."))
        col.addView(heroSub("Built for restricted and China SIM networks: unlisted bridges hide entry IPs, and country exclusions guarantee your traffic never exits where you don't want it."))

        // Exit Exclusion Card
        val exCard = cardContainer()
        exCard.addView(sectionLabel("NEVER EXIT THROUGH COUNTRIES"))
        val exSummary = TextView(this).apply {
            setTextColor(Color.parseColor("#CBD5E1"))
            textSize = 13.5f
            setPadding(0, dp(6), 0, dp(10))
        }
        fun refreshExSummary() {
            val set = prefs.getStringSet("exclude_cc", emptySet()) ?: emptySet()
            exSummary.text = if (set.isEmpty()) "Currently allowed: all exit countries."
            else "Excluded exit countries: " + set.sorted().joinToString(", ")
        }
        refreshExSummary()
        exCard.addView(exSummary)
        exCard.addView(actionButton("Choose Excluded Exit Countries") {
            showCountryExclusionDialog { refreshExSummary() }
        })
        col.addView(exCard)

        // Censorship Bridges Card
        val bridgeCard = cardContainer()
        bridgeCard.addView(sectionLabel("UNLISTED BRIDGES (CENSORED NETWORKS)"))
        val bridgeDesc = TextView(this).apply {
            text = "3 built-in unlisted bridges are always active. If your SIM or ISP blocks them, add extra `sail-bridge:…` lines below."
            setTextColor(Color.parseColor("#94A3B8"))
            textSize = 13f
            setPadding(0, dp(6), 0, dp(10))
        }
        bridgeCard.addView(bridgeDesc)
        bridgeCard.addView(actionButton("Edit Extra Bridge Lines") {
            showBridgesDialog()
        })
        col.addView(bridgeCard)

        // Pause when screen off
        val pauseCard = cardContainer()
        val row = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
        }
        val txtCol = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
        }
        txtCol.addView(sectionLabel("PAUSE WHEN SCREEN IS OFF"))
        txtCol.addView(TextView(this).apply {
            text = "Idles the circuit while your phone is locked so background apps spend zero XNO and zero battery. Resumes the instant you unlock."
            setTextColor(Color.parseColor("#94A3B8"))
            textSize = 12.5f
            setPadding(0, dp(4), dp(12), 0)
        })
        val sw = Switch(this).apply {
            isChecked = prefs.getBoolean("pause_hidden", true)
            setOnCheckedChangeListener { _, on ->
                prefs.edit().putBoolean("pause_hidden", on).apply()
            }
        }
        row.addView(txtCol, LinearLayout.LayoutParams(0, WRAP_CONTENT, 1f))
        row.addView(sw)
        pauseCard.addView(row)
        col.addView(pauseCard)

        return col
    }

    // ------------------------------------------------------------------------
    // Page 4: Share Connection (Wi-Fi Hotspot & Proxy)
    // ------------------------------------------------------------------------
    private fun buildHotspotSharePage(): View {
        val col = pageColumn()
        val prefs = PreferenceManager.getDefaultSharedPreferences(this)

        col.addView(stepBadge("4 OF 5 · SHARE CONNECTION"))
        col.addView(heroTitle("Put your laptop or another phone on the tunnel."))
        col.addView(heroSub("Android never routes hotspot guests through a VPN automatically. Sailnet solves this with a built-in hotspot proxy on port 8080."))

        val card = cardContainer()
        card.addView(sectionLabel("HOTSPOT SHARING MODE"))

        val shareModes = listOf(
            Triple("off", "Off", "Only this phone uses the circuit."),
            Triple("sailnet", "Sailnet Wi-Fi Hotspot (Zero-leak)", "Opens a dedicated local Wi-Fi hotspot with a scannable QR code. Guests without the proxy cannot leak to the clear internet."),
            Triple("mine", "My Android Hotspot", "Serves proxy port 8080 on your phone's standard Wi-Fi / USB / Bluetooth tethering interface.")
        )
        val cur = Share.mode(this)
        val rg = RadioGroup(this).apply {
            orientation = RadioGroup.VERTICAL
            setPadding(0, dp(6), 0, 0)
        }
        shareModes.forEachIndexed { idx, (key, title, desc) ->
            val rb = RadioButton(this).apply {
                id = 600 + idx
                text = "$title\n$desc"
                setTextColor(Color.parseColor("#E2E8F0"))
                textSize = 13.5f
                setPadding(dp(8), dp(10), dp(8), dp(10))
                isChecked = key == cur
            }
            rg.addView(rb)
        }
        rg.setOnCheckedChangeListener { _, checkedId ->
            val chosen = shareModes.getOrNull(checkedId - 600)?.first ?: "off"
            prefs.edit().putString("share", chosen).apply()
            Share.error = ""
            Share.apply(this)
        }
        card.addView(rg)
        col.addView(card)

        col.addView(featureCard(
            "Android Quick Settings Tile",
            "Pull down your Android notification shade and edit tiles to add the Sailnet tile — toggle the VPN with one tap from anywhere."
        ))
        return col
    }

    // ------------------------------------------------------------------------
    // Page 5: Self-Custody Wallet, Cost Meter & Identity
    // ------------------------------------------------------------------------
    private fun buildWalletAndIdentityPage(): View {
        val col = pageColumn()
        val addr = try { Mobile.address(filesDir.absolutePath) } catch (_: Exception) { "" }

        col.addView(stepBadge("5 OF 5 · WALLET, METER & IDENTITY"))
        col.addView(heroTitle("Your keys, your meter, zero logs."))
        col.addView(heroSub("A first-run trial is claimed automatically so you can connect right away. Every anchor tops up in place and your local cost ledger tracks every MiB."))

        // Nickname card
        val nickCard = cardContainer()
        nickCard.addView(sectionLabel("LOCAL PRIVACY NICKNAME"))
        val nickDesc = TextView(this).apply {
            text = "Current nickname: ${Prefs.nick(this@IntroActivity)}\nReplaces your wallet address and device IPs in every screen and log."
            setTextColor(Color.parseColor("#CBD5E1"))
            textSize = 13f
            setPadding(0, dp(6), 0, dp(10))
        }
        nickCard.addView(nickDesc)
        nickCard.addView(actionButton("Change Nickname") {
            val input = EditText(this).apply {
                setText(Prefs.nick(this@IntroActivity))
                setPadding(dp(20), dp(14), dp(20), dp(14))
            }
            AlertDialog.Builder(this)
                .setTitle("Privacy Nickname")
                .setMessage("Shown instead of your wallet address and IP in logs.")
                .setView(input)
                .setPositiveButton("Save") { _, _ ->
                    val n = input.text.toString().trim()
                    if (n.isNotEmpty()) {
                        Prefs.setNick(this, n)
                        nickDesc.text = "Current nickname: ${Prefs.nick(this)}\nReplaces your wallet address and device IPs in every screen and log."
                    }
                }
                .setNegativeButton("Cancel", null)
                .show()
        })
        col.addView(nickCard)

        // Wallet Backup & Restore card
        val walCard = cardContainer()
        walCard.addView(sectionLabel("SELF-CUSTODY NANO WALLET"))
        walCard.addView(TextView(this).apply {
            text = "Address:\n$addr\n\nYour 64-character seed lives only on this phone. Back it up so you never lose your balance on reinstall, or restore an existing wallet."
            setTextColor(Color.parseColor("#94A3B8"))
            textSize = 12.5f
            typeface = Typeface.MONOSPACE
            setPadding(0, dp(6), 0, dp(10))
        })
        val btnRow = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
        }
        btnRow.addView(actionButton("Back Up Seed") { showBackupDialog() }.apply {
            layoutParams = LinearLayout.LayoutParams(0, WRAP_CONTENT, 1f).apply { marginEnd = dp(6) }
        })
        btnRow.addView(actionButton("Restore Seed") { showRestoreDialog() }.apply {
            layoutParams = LinearLayout.LayoutParams(0, WRAP_CONTENT, 1f).apply { marginStart = dp(6) }
        })
        walCard.addView(btnRow)
        col.addView(walCard)

        return col
    }

    // ------------------------------------------------------------------------
    // Dialog Helpers
    // ------------------------------------------------------------------------
    private fun showCountryExclusionDialog(onUpdated: () -> Unit) {
        val prefs = PreferenceManager.getDefaultSharedPreferences(this)
        val codes = try { JSONArray(Mobile.countries()) } catch (_: Exception) { JSONArray() }
        val fallback = listOf("US", "DE", "SG", "MY", "IN", "ID", "BR", "GB", "AU", "CA", "CH", "NL")
        val list = ArrayList<String>()
        for (i in 0 until codes.length()) list.add(codes.getString(i))
        if (list.isEmpty()) list.addAll(fallback)
        val labels = list.map { Locale("", it).displayCountry.ifEmpty { it } + " ($it)" }.toTypedArray()
        val current = (prefs.getStringSet("exclude_cc", emptySet()) ?: emptySet()).toMutableSet()
        val checked = BooleanArray(list.size) { current.contains(list[it]) }

        AlertDialog.Builder(this)
            .setTitle("Never Exit Through")
            .setMultiChoiceItems(labels, checked) { _, which, isChecked ->
                if (isChecked) current.add(list[which]) else current.remove(list[which])
            }
            .setPositiveButton("Apply") { _, _ ->
                prefs.edit().putStringSet("exclude_cc", current).apply()
                try { Mobile.setExcludeExit(current.joinToString(",")) } catch (_: Exception) {}
                onUpdated()
            }
            .setNeutralButton("Clear All") { _, _ ->
                prefs.edit().putStringSet("exclude_cc", emptySet()).apply()
                try { Mobile.setExcludeExit("") } catch (_: Exception) {}
                onUpdated()
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    private fun showBridgesDialog() {
        val prefs = PreferenceManager.getDefaultSharedPreferences(this)
        val input = EditText(this).apply {
            hint = "sail-bridge:nano_…:ip:port:fp:host:secret"
            setText(prefs.getString("bridges", "") ?: "")
            setPadding(dp(20), dp(14), dp(20), dp(14))
            minLines = 3
        }
        AlertDialog.Builder(this)
            .setTitle("Extra Bridge Lines")
            .setMessage("Paste one `sail-bridge:…` line per line. Built-in bridges are always included automatically.")
            .setView(input)
            .setPositiveButton("Save") { _, _ ->
                prefs.edit().putString("bridges", input.text.toString().trim()).apply()
                Toast.makeText(this, "Bridge lines saved", Toast.LENGTH_SHORT).show()
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    private fun showBackupDialog() {
        val res = try { JSONObject(Mobile.exportWallet(filesDir.absolutePath)) } catch (_: Exception) { JSONObject() }
        if (!res.optBoolean("ok")) {
            Toast.makeText(this, res.optString("error", "Could not read wallet"), Toast.LENGTH_LONG).show()
            return
        }
        val seed = res.optString("seed")
        val addr = res.optString("address")
        AlertDialog.Builder(this)
            .setTitle("Back Up Your Wallet")
            .setMessage("Write down this 64-character seed and keep it off this phone. Anyone with it controls your XNO.\n\nAddress:\n$addr\n\nSeed:\n$seed")
            .setPositiveButton("Copy Seed") { _, _ ->
                getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("sailnet seed", seed))
                Toast.makeText(this, "Seed copied to clipboard", Toast.LENGTH_LONG).show()
            }
            .setNegativeButton("Done", null)
            .show()
    }

    private fun showRestoreDialog() {
        if (SailVpnService.running || SailVpnService.starting) {
            Toast.makeText(this, "Disconnect first before replacing the wallet.", Toast.LENGTH_LONG).show()
            return
        }
        val input = EditText(this).apply {
            hint = "Paste 64-character hex seed"
            setPadding(dp(20), dp(14), dp(20), dp(14))
            minLines = 2
        }
        AlertDialog.Builder(this)
            .setTitle("Restore a Wallet")
            .setMessage("Replaces the active wallet on this phone (the previous one is kept safely as a backup file).")
            .setView(input)
            .setPositiveButton("Restore") { _, _ ->
                val res = try { JSONObject(Mobile.importWallet(filesDir.absolutePath, input.text.toString())) } catch (_: Exception) { JSONObject() }
                if (res.optBoolean("ok")) {
                    Toast.makeText(this, "Restored: " + res.optString("address").take(20) + "…", Toast.LENGTH_LONG).show()
                } else {
                    Toast.makeText(this, res.optString("error", "Invalid seed"), Toast.LENGTH_LONG).show()
                }
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    private fun reconnectIfRunning() {
        if (!SailVpnService.running) return
        Toast.makeText(this, "Applying network mode…", Toast.LENGTH_SHORT).show()
        startService(Intent(this, SailVpnService::class.java).setAction(SailVpnService.ACTION_STOP))
        android.os.Handler(mainLooper).postDelayed({
            val i = Intent(this, SailVpnService::class.java)
            if (android.os.Build.VERSION.SDK_INT >= 26) startForegroundService(i) else startService(i)
        }, 1200)
    }

    // ------------------------------------------------------------------------
    // UI Styling Builders
    // ------------------------------------------------------------------------
    private fun pageColumn(): LinearLayout = LinearLayout(this).apply {
        orientation = LinearLayout.VERTICAL
        setPadding(dp(20), dp(12), dp(20), dp(24))
    }

    private fun stepBadge(text: String): TextView = TextView(this).apply {
        this.text = text
        setTextColor(Color.parseColor("#14B8A6"))
        textSize = 11f
        typeface = Typeface.create(Typeface.DEFAULT, Typeface.BOLD)
        letterSpacing = 0.12f
        setPadding(0, dp(4), 0, dp(6))
    }

    private fun heroTitle(text: String): TextView = TextView(this).apply {
        this.text = text
        setTextColor(Color.WHITE)
        textSize = 24f
        typeface = Typeface.create(Typeface.DEFAULT, Typeface.BOLD)
        setPadding(0, 0, 0, dp(8))
    }

    private fun heroSub(text: String): TextView = TextView(this).apply {
        this.text = text
        setTextColor(Color.parseColor("#94A3B8"))
        textSize = 14f
        setLineSpacing(0f, 1.25f)
        setPadding(0, 0, 0, dp(14))
    }

    private fun sectionLabel(text: String): TextView = TextView(this).apply {
        this.text = text
        setTextColor(Color.parseColor("#14B8A6"))
        textSize = 11f
        typeface = Typeface.create(Typeface.DEFAULT, Typeface.BOLD)
        letterSpacing = 0.1f
    }

    private fun cardContainer(): LinearLayout = LinearLayout(this).apply {
        orientation = LinearLayout.VERTICAL
        background = roundedBg("#0F172A", "#1E293B", dp(14))
        setPadding(dp(16), dp(16), dp(16), dp(16))
        layoutParams = LinearLayout.LayoutParams(MATCH_PARENT, WRAP_CONTENT).apply {
            bottomMargin = dp(12)
        }
    }

    private fun featureCard(title: String, body: String): View = cardContainer().apply {
        addView(TextView(this@IntroActivity).apply {
            text = title
            setTextColor(Color.WHITE)
            textSize = 15.5f
            typeface = Typeface.create(Typeface.DEFAULT, Typeface.BOLD)
            setPadding(0, 0, 0, dp(4))
        })
        addView(TextView(this@IntroActivity).apply {
            text = body
            setTextColor(Color.parseColor("#CBD5E1"))
            textSize = 13.5f
            setLineSpacing(0f, 1.25f)
        })
    }

    private fun actionButton(label: String, onClick: () -> Unit): Button = Button(this).apply {
        text = label
        isAllCaps = false
        setTextColor(Color.parseColor("#5EEAD4"))
        textSize = 13.5f
        typeface = Typeface.create(Typeface.DEFAULT, Typeface.BOLD)
        background = roundedBg("#132A2E", "#0D9488", dp(10))
        setPadding(dp(14), dp(10), dp(14), dp(10))
        setOnClickListener { onClick() }
    }

    private fun roundedBg(fillHex: String, strokeHex: String, radiusPx: Int): GradientDrawable =
        GradientDrawable().apply {
            shape = GradientDrawable.RECTANGLE
            cornerRadius = radiusPx.toFloat()
            setColor(Color.parseColor(fillHex))
            setStroke(dp(1), Color.parseColor(strokeHex))
        }

    private fun pillBg(fillHex: String, strokeHex: String): GradientDrawable =
        roundedBg(fillHex, strokeHex, dp(24))

    private fun dp(v: Int): Int = (v * resources.displayMetrics.density).toInt()
}
