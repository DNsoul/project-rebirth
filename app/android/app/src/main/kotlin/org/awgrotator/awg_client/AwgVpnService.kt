package org.awgrotator.awg_client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.VpnService
import android.os.Build

/**
 * Holds the system's VPN interface. It does not move any traffic itself: it asks
 * the system for an interface and hands the file descriptor to the tunnel library
 * (libawg.so, driven from Dart), which owns it from then on.
 *
 * Asking for a new interface while one exists replaces it, which is how the app
 * moves from one server to the next with only the shortest gap.
 *
 * The service runs in the foreground with a notification that says what the
 * connection is doing. Phones that save battery by stopping background programs
 * (Xiaomi's among them) leave a foreground service alone.
 */
class AwgVpnService : VpnService() {

    private var status = "VPN работает"

    override fun onCreate() {
        super.onCreate()
        instance = this
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        goForeground()
        // If the system ends the service, nothing restarts it by itself: the app
        // decides when there is a tunnel.
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        instance = null
        super.onDestroy()
    }

    /** The user took the permission away, or another VPN app took over. */
    override fun onRevoke() {
        onRevoked?.invoke()
        super.onRevoke()
    }

    /** Changes what the notification says. */
    fun setStatus(text: String) {
        status = text
        getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, notification())
    }

    private fun goForeground() {
        if (Build.VERSION.SDK_INT >= 26) {
            getSystemService(NotificationManager::class.java).createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "VPN", NotificationManager.IMPORTANCE_LOW)
            )
        }
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(
                NOTIFICATION_ID,
                notification(),
                ServiceInfo.FOREGROUND_SERVICE_TYPE_SYSTEM_EXEMPTED,
            )
        } else {
            startForeground(NOTIFICATION_ID, notification())
        }
    }

    private fun notification(): Notification {
        val open = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val builder = if (Build.VERSION.SDK_INT >= 26) {
            Notification.Builder(this, CHANNEL_ID)
        } else {
            @Suppress("DEPRECATION")
            Notification.Builder(this)
        }
        return builder
            .setContentTitle("Rebirth")
            .setContentText(status)
            .setSmallIcon(R.drawable.ic_stat_rebirth)
            .setContentIntent(open)
            .setOngoing(true)
            .build()
    }

    /**
     * Opens an interface with the given addresses, routes and name servers and
     * returns its descriptor, detached: the caller owns it and must close it.
     */
    fun establishTun(
        session: String,
        addresses: List<String>,
        routes: List<String>,
        dns: List<String>,
        mtu: Int,
    ): Int {
        val builder = Builder().setSession(session).setMtu(mtu)
        for (a in addresses) {
            val (ip, len) = split(a)
            builder.addAddress(ip, len)
        }
        for (r in routes) {
            val (ip, len) = split(r)
            builder.addRoute(ip, len)
        }
        for (d in dns) builder.addDnsServer(d)

        val pfd = builder.establish()
            ?: throw IllegalStateException("the system refused to open the VPN interface (is the permission still granted?)")
        return pfd.detachFd()
    }

    companion object {
        private const val CHANNEL_ID = "awg-vpn"
        private const val NOTIFICATION_ID = 1

        @Volatile
        var instance: AwgVpnService? = null

        /** Called when the system takes the VPN away from the app. */
        @Volatile
        var onRevoked: (() -> Unit)? = null

        /** Starts the service if it is not running and waits until it is. */
        fun start(context: Context): AwgVpnService {
            instance?.let { return it }
            context.startService(Intent(context, AwgVpnService::class.java))
            val deadline = System.currentTimeMillis() + 5_000
            while (System.currentTimeMillis() < deadline) {
                instance?.let { return it }
                Thread.sleep(25)
            }
            throw IllegalStateException("the VPN service did not start")
        }

        fun release() {
            instance?.let {
                it.stopForeground(STOP_FOREGROUND_REMOVE)
                it.stopSelf()
            }
        }

        private fun split(cidr: String): Pair<String, Int> {
            val i = cidr.indexOf('/')
            return if (i < 0) Pair(cidr, if (cidr.contains(':')) 128 else 32)
            else Pair(cidr.substring(0, i), cidr.substring(i + 1).toInt())
        }
    }
}
