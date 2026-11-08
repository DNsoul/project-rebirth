package org.awgrotator.awg_client

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.provider.Settings
import androidx.core.app.NotificationManagerCompat
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
    private var pendingNotifications: MethodChannel.Result? = null
    private var pendingBattery: MethodChannel.Result? = null
    private var waitingForSettings: MethodChannel.Result? = null
    private var settingsAnswer: () -> String = { "denied" }
    private var leftForSettings = false
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

            "notifications" -> ensureNotifications(result)

            "battery" -> ensureBatteryExemption(result)

            "install" -> ensureInstallPermission(result)

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
        if (Build.VERSION.SDK_INT >= 26 && !packageManager.canRequestPackageInstalls()) {
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

    /**
     * Makes sure the notification that says what the VPN is doing can be seen.
     * Answers "granted", "denied", or "settings" (the person was sent to the page of
     * the app's settings and has come back; they may or may not have switched it on).
     *
     * From Android 13 the system asks "allow notifications?" in a dialog of its own.
     * It stops asking after two refusals, and then the only way left is the page of
     * the app's settings, which is opened at most once a day. Before
     * Android 13 there is no dialog: notifications are on unless they were turned
     * off, and some phones (Xiaomi's) turn them off for programs that did not come
     * from the store. There the page of the settings is the only way too.
     */
    private fun ensureNotifications(result: MethodChannel.Result) {
        if (NotificationManagerCompat.from(this).areNotificationsEnabled()) {
            result.success("granted")
            return
        }
        if (pendingNotifications != null || waitingForSettings != null) {
            result.error("busy", "notifications are already being asked for", null)
            return
        }
        val prefs = getSharedPreferences("permissions", MODE_PRIVATE)
        if (Build.VERSION.SDK_INT >= 33) {
            val asked = prefs.getBoolean(ASKED_NOTIFICATIONS, false)
            // After the first refusal the system explains and asks once more; after
            // the second it says no without a word, and nothing is shown.
            if (!asked || shouldShowRequestPermissionRationale(Manifest.permission.POST_NOTIFICATIONS)) {
                prefs.edit().putBoolean(ASKED_NOTIFICATIONS, true).apply()
                pendingNotifications = result
                requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQUEST_NOTIFICATIONS)
                return
            }
        }
        // No dialog to show. The page of the settings, not more than once a day.
        if (!dueAgain(NOTIFICATION_SETTINGS_ASKED_AT)) {
            result.success("denied")
            return
        }
        openSettings(
            Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                .putExtra(Settings.EXTRA_APP_PACKAGE, packageName),
            result,
        ) { if (NotificationManagerCompat.from(this).areNotificationsEnabled()) "granted" else "settings" }
    }

    /**
     * Lets the app run in the background without the system's battery saving
     * stopping it. A VPN that is stopped when the screen goes off is no VPN, and
     * phones that save battery hard (Xiaomi's among them) do it to programs that
     * have not been exempted. The system asks "allow always running in the
     * background?" in a dialog of its own. Answers "granted" or "denied".
     *
     * Asked at every start while the app is not exempt: it is the one thing that
     * decides whether the VPN survives the screen going off.
     */
    private fun ensureBatteryExemption(result: MethodChannel.Result) {
        val power = getSystemService(PowerManager::class.java)
        if (power.isIgnoringBatteryOptimizations(packageName)) {
            result.success("granted")
            return
        }
        if (pendingBattery != null) {
            result.error("busy", "the battery question is already being asked", null)
            return
        }
        pendingBattery = result
        try {
            @Suppress("DEPRECATION")
            startActivityForResult(
                Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName")),
                REQUEST_BATTERY,
            )
        } catch (e: Exception) {
            pendingBattery = null
            result.success("denied")
        }
    }

    /**
     * Lets the app install the updates it downloads. Android has no dialog for
     * this, only the page "install unknown apps" for the app, so it is opened (at
     * most once a day), and the answer comes when the person is back. Answers
     * "granted" or "denied".
     */
    private fun ensureInstallPermission(result: MethodChannel.Result) {
        // Before Android 8 it is one switch for all programs, not one for each.
        if (Build.VERSION.SDK_INT < 26 || packageManager.canRequestPackageInstalls()) {
            result.success("granted")
            return
        }
        if (pendingBattery != null || waitingForSettings != null) {
            result.error("busy", "a question is already being asked", null)
            return
        }
        if (!dueAgain(INSTALL_SETTINGS_ASKED_AT)) {
            result.success("denied")
            return
        }
        openSettings(
            Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:$packageName")),
            result,
        ) { if (packageManager.canRequestPackageInstalls()) "granted" else "denied" }
    }

    /**
     * Opens a page of the system's settings and answers [result] when the person
     * is back, with whatever [answer] says then. The answer waits, so that the next
     * question does not appear under the page.
     */
    private fun openSettings(intent: Intent, result: MethodChannel.Result, answer: () -> String) {
        try {
            settingsAnswer = answer
            waitingForSettings = result
            startActivity(intent)
        } catch (e: Exception) {
            waitingForSettings = null
            result.success("denied")
        }
    }

    /**
     * True if the question may be asked now, and notes the moment, so that it is not
     * asked again for a day. For pages of the settings: opening one at every start
     * would be a nuisance, but a person who went back by mistake should be asked
     * again, and not never.
     */
    private fun dueAgain(key: String): Boolean {
        val prefs = getSharedPreferences("permissions", MODE_PRIVATE)
        val now = System.currentTimeMillis()
        if (now - prefs.getLong(key, 0) < ASK_AGAIN_AFTER_MS) return false
        prefs.edit().putLong(key, now).apply()
        return true
    }

    /**
     * Tells when a page of the settings has been left. The window of this app loses
     * the focus when the page comes up and gets it back when the page is gone: unlike
     * onResume, which also runs for other reasons (a dialog that has just closed) and
     * could answer while the page is still in front, and let the next question be
     * asked under it.
     */
    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        val waiting = waitingForSettings ?: return
        if (!hasFocus) {
            leftForSettings = true
        } else if (leftForSettings) {
            waitingForSettings = null
            leftForSettings = false
            // The system takes a moment to apply what the person switched.
            ui.postDelayed({ waiting.success(settingsAnswer()) }, SETTLE_MS)
        }
    }

    @Suppress("DEPRECATION")
    override fun onRequestPermissionsResult(
        requestCode: Int,
        permissions: Array<out String>,
        grantResults: IntArray,
    ) {
        if (requestCode == REQUEST_NOTIFICATIONS) {
            val granted = grantResults.isNotEmpty() && grantResults[0] == PackageManager.PERMISSION_GRANTED
            pendingNotifications?.success(if (granted) "granted" else "denied")
            pendingNotifications = null
            return
        }
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
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
        if (requestCode == REQUEST_BATTERY) {
            val pending = pendingBattery
            pendingBattery = null
            // The system applies the choice a moment after the dialog closes (Xiaomi's
            // power page does), so asking at once gets the old answer.
            ui.postDelayed({
                val power = getSystemService(PowerManager::class.java)
                val exempt = resultCode == Activity.RESULT_OK || power.isIgnoringBatteryOptimizations(packageName)
                pending?.success(if (exempt) "granted" else "denied")
            }, SETTLE_MS)
            return
        }
        super.onActivityResult(requestCode, resultCode, data)
    }

    companion object {
        private const val CHANNEL = "awg/vpn"
        private const val REQUEST_VPN = 0x7670
        private const val REQUEST_NOTIFICATIONS = 0x6e74
        private const val REQUEST_BATTERY = 0x6274
        private const val ASKED_NOTIFICATIONS = "asked_notifications"
        private const val NOTIFICATION_SETTINGS_ASKED_AT = "notification_settings_asked_at"
        private const val INSTALL_SETTINGS_ASKED_AT = "install_settings_asked_at"
        private const val ASK_AGAIN_AFTER_MS = 24L * 60 * 60 * 1000
        private const val SETTLE_MS = 600L
    }
}
