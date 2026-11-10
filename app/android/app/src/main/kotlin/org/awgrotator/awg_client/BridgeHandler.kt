package org.awgrotator.awg_client

import android.content.Context
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import io.flutter.plugin.common.BinaryMessenger
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.util.concurrent.Executors

/**
 * What the Dart program asks of Android, answered without a screen: the VPN
 * interface, keeping sockets out of it, the notification, the device's id.
 * These work when the app has been closed from the list of recent apps and only
 * the service is left, which is when the program that looks after the tunnel
 * still has to be able to bring it up again.
 *
 * What needs a screen (asking for a permission, handing a download to the
 * installer) goes to the one that is open, if there is one.
 */
class BridgeHandler(
    private val app: Context,
    messenger: BinaryMessenger,
) : MethodChannel.MethodCallHandler {

    private val channel = MethodChannel(messenger, CHANNEL)
    private val worker = Executors.newSingleThreadExecutor()
    private val ui = Handler(Looper.getMainLooper())

    init {
        channel.setMethodCallHandler(this)
        // These belong to the service and so to the process, not to a screen: a
        // screen that is gone must not stop the program hearing about them.
        AwgVpnService.onRevoked = { ui.post { channel.invokeMethod("revoked", null) } }
        AwgVpnService.onNetworkChanged = { available ->
            ui.post { channel.invokeMethod("networkChanged", mapOf("available" to available)) }
        }
    }

    override fun onMethodCall(call: MethodCall, result: MethodChannel.Result) {
        when (call.method) {
            "prepare" -> {
                // Permission already given: nothing to show, so no screen is needed.
                if (VpnService.prepare(app) == null) result.success(true) else toScreen(call, result)
            }

            "establish" -> {
                val session = call.argument<String>("session") ?: "AWG"
                val addresses = call.argument<List<String>>("addresses") ?: emptyList()
                val routes = call.argument<List<String>>("routes") ?: emptyList()
                val dns = call.argument<List<String>>("dns") ?: emptyList()
                val mtu = call.argument<Int>("mtu") ?: 1280
                // Starting the service waits for it, so this is not done on the
                // thread that draws the screen.
                worker.execute {
                    try {
                        val fd = AwgVpnService.start(app)
                            .establishTun(session, addresses, routes, dns, mtu)
                        ui.post { result.success(fd) }
                    } catch (e: Exception) {
                        ui.post { result.error("establish", e.message, null) }
                    }
                }
            }

            "protect" -> {
                val fd = call.argument<Int>("fd") ?: -1
                result.success(AwgVpnService.instance?.protect(fd) ?: false)
            }

            "release" -> {
                AwgVpnService.release()
                result.success(null)
            }

            "status" -> {
                // What the notification says. There is nothing to update when the
                // service is not running, which is not a failure.
                AwgVpnService.instance?.setStatus(call.argument<String>("text") ?: "")
                result.success(null)
            }

            "androidId" ->
                result.success(Settings.Secure.getString(app.contentResolver, Settings.Secure.ANDROID_ID))

            // What the owner sees next to a request to come in. Many makers
            // repeat their name in the model ("Xiaomi" + "Xiaomi 13"), so it is
            // given once.
            "deviceModel" -> {
                val maker = Build.MANUFACTURER.orEmpty().trim()
                val model = Build.MODEL.orEmpty().trim()
                result.success(
                    if (maker.isEmpty() || model.startsWith(maker, ignoreCase = true)) model
                    else "$maker $model",
                )
            }

            "filesDir" -> result.success(app.filesDir.absolutePath)

            else -> toScreen(call, result)
        }
    }

    private fun toScreen(call: MethodCall, result: MethodChannel.Result) {
        val screen = ScreenHooks.current
        if (screen == null) {
            result.error("no_screen", "${call.method} needs the screen, and none is open", null)
        } else {
            screen.handleScreenCall(call, result)
        }
    }

    companion object {
        const val CHANNEL = "awg/vpn"
    }
}

/** The screen that is open, if any: where the calls that need one are sent. */
object ScreenHooks {
    @Volatile
    var current: MainActivity? = null
}
