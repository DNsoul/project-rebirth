package org.awgrotator.awg_client

import android.content.Context
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugins.GeneratedPluginRegistrant

/**
 * The one Flutter engine of the app, kept for as long as the process lives.
 *
 * The Dart program is what looks after the tunnel: it checks that the server is
 * still answering, moves to a new one, and follows the phone from Wi-Fi to mobile
 * data. The tunnel itself and the VPN service outlive the screen (the service is
 * in the foreground, so the system keeps the process), and so the program that
 * looks after them has to as well. With an engine made for each screen, closing
 * the app from the list of recent apps took the program away and left the tunnel
 * running with nobody to look after it; opening the app again started a new
 * program that knew nothing about it, and the first press of the button opened a
 * second tunnel beside the first, with the same key. Two tunnels with one key
 * make the server answer whichever spoke last, and half of what comes back is
 * lost.
 *
 * The screen attaches to this engine when it opens and lets go when it closes.
 * The Dart program starts when the first screen does, as it always has, and
 * carries on when there is none.
 */
object RebirthEngine {
    private var engine: FlutterEngine? = null

    // Held so that it is not collected: it is what the Dart side talks to.
    @Suppress("unused")
    private var bridge: BridgeHandler? = null

    @Synchronized
    fun get(context: Context): FlutterEngine {
        engine?.let { return it }
        val app = context.applicationContext
        val created = FlutterEngine(app)
        // The screen does not register them for an engine it was handed.
        GeneratedPluginRegistrant.registerWith(created)
        bridge = BridgeHandler(app, created.dartExecutor.binaryMessenger)
        engine = created
        return created
    }
}
