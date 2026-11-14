package org.awgrotator.awg_client

import android.Manifest
import android.content.ContentValues
import android.content.Context
import android.content.pm.PackageManager
import android.media.MediaScannerConnection
import android.os.Build
import android.os.Environment
import android.provider.MediaStore
import io.flutter.plugin.common.MethodChannel
import java.io.File
import java.io.IOException

/**
 * Puts a file into the shared Downloads folder, where a person finds it in the
 * Files app and can send it on.
 *
 * From Android 10 it goes in through MediaStore, which needs no permission. On
 * Android 7–9 the file is written into the folder itself, which needs the right
 * to write to the shared storage; the screen asks for it ([MainActivity]) when
 * it is missing, and the file is then shown to the media index so that the
 * Files app sees it at once.
 */
object Downloads {
    /** True when a file can be saved without asking anything. */
    fun canWrite(context: Context): Boolean =
        Build.VERSION.SDK_INT >= 29 ||
            context.checkSelfPermission(Manifest.permission.WRITE_EXTERNAL_STORAGE) ==
            PackageManager.PERMISSION_GRANTED

    /** Saves [bytes] as [name] and answers [result] with the name it got. Not on the UI thread. */
    fun saveAndAnswer(context: Context, name: String, bytes: ByteArray, answer: (Result<String>) -> Unit) {
        try {
            val saved = save(context, name, bytes)
            NativeLog.i("journal saved to Downloads as $saved")
            answer(Result.success(saved))
        } catch (e: Exception) {
            NativeLog.e("journal not saved", e)
            answer(Result.failure(e))
        }
    }

    /** The error code the Dart side turns into words for the person. */
    fun codeFor(e: Throwable): String = when {
        e is SecurityException -> "denied"
        (e.message ?: "").contains("ENOSPC") || (e.message ?: "").contains("No space", ignoreCase = true) -> "no_space"
        else -> "failed"
    }

    fun reply(result: MethodChannel.Result, outcome: Result<String>) {
        outcome.fold(
            { result.success(it) },
            { result.error(codeFor(it), it.message, null) },
        )
    }

    private fun save(context: Context, name: String, bytes: ByteArray): String =
        if (Build.VERSION.SDK_INT >= 29) viaMediaStore(context, name, bytes) else intoFolder(context, name, bytes)

    @android.annotation.TargetApi(29)
    private fun viaMediaStore(context: Context, name: String, bytes: ByteArray): String {
        val resolver = context.contentResolver
        val values = ContentValues().apply {
            put(MediaStore.Downloads.DISPLAY_NAME, name)
            put(MediaStore.Downloads.MIME_TYPE, "text/plain")
            put(MediaStore.Downloads.IS_PENDING, 1)
        }
        val uri = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
            ?: throw IOException("the system would not make a file in Downloads")
        try {
            val out = resolver.openOutputStream(uri) ?: throw IOException("the file in Downloads could not be opened")
            out.use { it.write(bytes) }
            values.clear()
            values.put(MediaStore.Downloads.IS_PENDING, 0)
            resolver.update(uri, values, null, null)
        } catch (e: Exception) {
            resolver.delete(uri, null, null)
            throw e
        }
        // The system renames it if the name is taken: "name (1).txt".
        resolver.query(uri, arrayOf(MediaStore.Downloads.DISPLAY_NAME), null, null, null)?.use { c ->
            if (c.moveToFirst()) return c.getString(0) ?: name
        }
        return name
    }

    @Suppress("DEPRECATION")
    private fun intoFolder(context: Context, name: String, bytes: ByteArray): String {
        val dir = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
        if (!dir.isDirectory && !dir.mkdirs()) throw IOException("there is no Downloads folder")
        val dot = name.lastIndexOf('.')
        val stem = if (dot < 0) name else name.substring(0, dot)
        val ext = if (dot < 0) "" else name.substring(dot)
        var file = File(dir, name)
        var i = 2
        while (file.exists()) file = File(dir, "$stem ($i)$ext").also { i++ }
        file.writeBytes(bytes)
        MediaScannerConnection.scanFile(context, arrayOf(file.path), arrayOf("text/plain"), null)
        return file.name
    }
}
