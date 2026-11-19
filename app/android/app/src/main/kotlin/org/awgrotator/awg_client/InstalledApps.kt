package org.awgrotator.awg_client

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.content.pm.ResolveInfo
import android.os.Build

/**
 * The apps on the phone, for choosing which go past the tunnel (the choice is
 * made on the Dart side, in ru_apps.dart), and word when one is installed or
 * removed.
 *
 * Only apps with something to start from the home screen are listed: those are
 * the apps a person uses, and Android 11 and later show an app no others unless
 * it asks for every package (the manifest asks for these). A package without
 * one is neither listed nor left out of the VPN.
 */
object InstalledApps {
    @Volatile
    private var listener: (() -> Unit)? = null
    private var receiver: BroadcastReceiver? = null

    /** Package name and the store that installed it, if the system says. */
    fun list(context: Context): List<Map<String, String?>> {
        val pm = context.packageManager
        val launchable = Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER)
        val found: List<ResolveInfo> = if (Build.VERSION.SDK_INT >= 33) {
            pm.queryIntentActivities(launchable, PackageManager.ResolveInfoFlags.of(0))
        } else {
            @Suppress("DEPRECATION")
            pm.queryIntentActivities(launchable, 0)
        }
        val packages = found.map { it.activityInfo.packageName }.toSortedSet()
        packages.remove(context.packageName)
        return packages.map { mapOf("package" to it, "installer" to installer(pm, it)) }
    }

    private fun installer(pm: PackageManager, pkg: String): String? = try {
        if (Build.VERSION.SDK_INT >= 30) {
            pm.getInstallSourceInfo(pkg).installingPackageName
        } else {
            @Suppress("DEPRECATION")
            pm.getInstallerPackageName(pkg)
        }
    } catch (e: Exception) {
        null
    }

    /**
     * Calls [onChange] when an app is installed or removed, for as long as the
     * process lives. A later call replaces the listener. Updates of an app that
     * is already there are not reported: the app stays what it was.
     */
    @Synchronized
    fun watch(context: Context, onChange: () -> Unit) {
        listener = onChange
        if (receiver != null) return
        val r = object : BroadcastReceiver() {
            override fun onReceive(c: Context, intent: Intent) {
                if (intent.getBooleanExtra(Intent.EXTRA_REPLACING, false)) return
                listener?.invoke()
            }
        }
        val filter = IntentFilter().apply {
            addAction(Intent.ACTION_PACKAGE_ADDED)
            addAction(Intent.ACTION_PACKAGE_REMOVED)
            addDataScheme("package")
        }
        // Sent by the system only, which reaches a receiver closed to other apps.
        if (Build.VERSION.SDK_INT >= 33) {
            context.registerReceiver(r, filter, Context.RECEIVER_NOT_EXPORTED)
        } else {
            context.registerReceiver(r, filter)
        }
        receiver = r
    }
}
