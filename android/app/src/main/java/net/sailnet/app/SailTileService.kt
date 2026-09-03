package net.sailnet.app

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService

/** Quick Settings tile: one tap to connect or disconnect. */
class SailTileService : TileService() {
    override fun onStartListening() = update()

    override fun onClick() {
        if (SailVpnService.running) {
            startService(Intent(this, SailVpnService::class.java).setAction(SailVpnService.ACTION_STOP))
        } else if (VpnService.prepare(this) == null) {
            val i = Intent(this, SailVpnService::class.java)
            if (Build.VERSION.SDK_INT >= 26) startForegroundService(i) else startService(i)
        } else {
            startActivityAndCollapse(Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        }
        update()
    }

    private fun update() {
        val t = qsTile ?: return
        t.state = if (SailVpnService.running) Tile.STATE_ACTIVE else Tile.STATE_INACTIVE
        t.label = "Sailnet"
        t.updateTile()
    }

    companion object {
        fun refresh(ctx: Context) {
            try { requestListeningState(ctx, ComponentName(ctx, SailTileService::class.java)) } catch (_: Exception) {}
        }
    }
}
