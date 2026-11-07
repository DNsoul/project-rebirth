package org.awgrotator.awg_client

import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.net.VpnService
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import androidx.core.content.FileProvider
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.io.File
import java.util.concurrent.Executors

/**
 * The bridge between the Dart code and what only Android can do: ask the user
 * for the VPN permission, get a VPN interface, keep sockets out of it, say which
 * device this is. The tunnel itself runs in libawg.so, which Dart calls directly.
 */
class MainActivity : FlutterActivity() {
    private val worker = Executors.newSingleThreadExecutor()
    private val ui = Handler(Looper.getMainLooper())
    private var pendingPermission: MethodChannel.Result? = null
    private lateinit var channel: MethodChannel

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        channel = MethodChannel(flutterEngine.dartExecutor.binaryMessenger, CHANNEL)
        AwgVpnService.onRevoked = { ui.post { channel.invokeMethod("revoked", null) } }
        channel.setMethodCallHandler { call, result -> handle(call, result) }
    }

    override fun onDestroy() {
        // The Dart side that would hear about a revocation is gone with the
        // engine; the service itself carries on.
        AwgVpnService.onRevoked = null
        super.onDestroy()
    }

    private fun handle(call: MethodCall, result: MethodChannel.Result) {
        when (call.method) {
            "prepare" -> prepare(result)

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
                        val fd = AwgVpnService.start(applicationContext)
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
                result.success(Settings.Secure.getString(contentResolver, Settings.Secure.ANDROID_ID))

            "filesDir" -> result.success(filesDir.absolutePath)

            "installApk" -> installApk(call.argument<String>("path") ?: "", result)

            else -> result.notImplemented()
        }
    }

    /**
     * Hands a downloaded package to the system's installer. Android lets an app do
     * that only once the person has allowed it for this app in the settings, so
     * the first time this opens that page instead and says so; the Dart side tells
     * the person to come back and press the button again.
     *
     * The file must be under filesDir/updates: that is the only place the file
     * provider shares from (res/xml/update_paths.xml).
     */
    private fun installApk(path: String, result: MethodChannel.Result) {
        val file = File(path)
        if (!file.isFile) {
            result.error("installApk", "the file is not there: $path", null)
            return
        }
        if (!packageManager.canRequestPackageInstalls()) {
            startActivity(
                Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:$packageName"))
            )
            result.success("needs_permission")
            return
        }
        try {
            val uri = FileProvider.getUriForFile(this, "$packageName.updates", file)
            val intent = Intent(Intent.ACTION_VIEW)
                .setDataAndType(uri, "application/vnd.android.package-archive")
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            startActivity(intent)
            result.success("started")
        } catch (e: Exception) {
            result.error("installApk", e.message, null)
        }
    }

    /** Asks for the VPN permission if the app does not have it yet. */
    private fun prepare(result: MethodChannel.Result) {
        val intent = VpnService.prepare(this)
        if (intent == null) {
            result.success(true)
            return
        }
        if (pendingPermission != null) {
            result.error("busy", "the permission is already being asked for", null)
            return
        }
        pendingPermission = result
        @Suppress("DEPRECATION")
        startActivityForResult(intent, REQUEST_VPN)
    }

    @Suppress("DEPRECATION")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        if (requestCode == REQUEST_VPN) {
            pendingPermission?.success(resultCode == Activity.RESULT_OK)
            pendingPermission = null
            return
        }
        super.onActivityResult(requestCode, resultCode, data)
    }

    companion object {
        private const val CHANNEL = "awg/vpn"
        private const val REQUEST_VPN = 0x7670
    }
}
