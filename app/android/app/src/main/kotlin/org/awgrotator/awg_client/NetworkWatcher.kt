package org.awgrotator.awg_client

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Handler
import android.os.Looper
import java.net.Inet4Address

/**
 * Tells when the phone has moved to another network (Wi-Fi to mobile data and
 * back, a different Wi-Fi, no network at all), and says nothing else.
 *
 * The system reports a great deal more: every change in signal strength, every
 * renewed address lease. Most of that changes nothing for the tunnel, and acting
 * on all of it would make it start over every few seconds. So reports are held for
 * a moment, and then the networks the phone could use are compared with how they
 * were at the last report; only a difference is passed on.
 *
 * Networks are named by what a person would call a change: which networks there
 * are, the interface each is on, and its IPv4 addresses. IPv6 addresses are left
 * out on purpose: privacy extensions replace them every few hours on a network
 * that has not changed at all.
 */
class NetworkWatcher(
    context: Context,
    private val onChange: (available: Boolean) -> Unit,
) {
    private val cm = context.getSystemService(ConnectivityManager::class.java)
    private val handler = Handler(Looper.getMainLooper())
    private var callback: ConnectivityManager.NetworkCallback? = null
    private var last = ""

    private val settled = Runnable { compare() }

    fun start() {
        if (callback != null) return
        // How things are now is the starting point, not a change.
        last = snapshot()
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) = hold()
            override fun onLost(network: Network) = hold()
            override fun onCapabilitiesChanged(network: Network, capabilities: NetworkCapabilities) = hold()
            override fun onLinkPropertiesChanged(network: Network, linkProperties: LinkProperties) = hold()
        }
        // The networks the phone itself reaches the internet by, not the VPN
        // that sits on top of them.
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
            .build()
        try {
            cm.registerNetworkCallback(request, cb)
            callback = cb
        } catch (e: SecurityException) {
            // Without the right to watch there is nothing to report; the tunnel
            // then relies on its own timers, as it did before.
        }
    }

    fun stop() {
        handler.removeCallbacks(settled)
        callback?.let {
            try {
                cm.unregisterNetworkCallback(it)
            } catch (e: IllegalArgumentException) {
                // Already gone.
            }
        }
        callback = null
    }

    private fun hold() {
        handler.removeCallbacks(settled)
        handler.postDelayed(settled, HOLD_MS)
    }

    private fun compare() {
        val now = snapshot()
        if (now == last) return
        last = now
        onChange(now.isNotEmpty())
    }

    /** One line for each network that can reach the internet, sorted. */
    @Suppress("DEPRECATION")
    private fun snapshot(): String {
        val lines = ArrayList<String>()
        for (n in cm.allNetworks) {
            val caps = cm.getNetworkCapabilities(n) ?: continue
            if (!caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) continue
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) continue
            val props = cm.getLinkProperties(n) ?: continue
            val v4 = props.linkAddresses
                .map { it.address }
                .filterIsInstance<Inet4Address>()
                .mapNotNull { it.hostAddress }
                .sorted()
            lines.add("$n ${props.interfaceName} ${v4.joinToString(",")}")
        }
        lines.sort()
        return lines.joinToString(";")
    }

    private companion object {
        // Long enough to fold the burst of reports a switch makes into one,
        // short enough that the tunnel is nudged while the person still waits.
        const val HOLD_MS = 700L
    }
}
