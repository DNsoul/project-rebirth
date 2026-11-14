package org.awgrotator.awg_client

import android.content.Context
import android.util.Log
import java.io.BufferedReader
import java.io.File
import java.io.InputStreamReader
import java.text.SimpleDateFormat
import java.util.TimeZone
import java.util.Date
import java.util.Locale

/**
 * The Android side's journal: what the VPN service and the network watcher do,
 * what the tunnel library (libawg.so) says, and a crash of the process, in a
 * file in the app's folder (native.log). The Dart side keeps its own (app.log)
 * in the same format and puts the two together, in the order things happened,
 * when the journal is saved into Downloads by a long press on the button.
 *
 * The tunnel library writes to the system log (logcat) under the tag "awg"; an
 * app may read its own lines there without any permission, so they are read
 * back as they come and copied in. Nothing has to change in the library.
 *
 * Like the Dart side's, the file stops at [MAX_BYTES], becomes native.log.1 and
 * starts again, and every line is scrubbed of keys and the servers' addresses
 * before it is written ([Scrub]).
 */
object NativeLog {
    private const val TAG = "Rebirth"
    private const val MAX_BYTES = 512L * 1024
    private const val FILE = "native.log"

    private var file: File? = null
    private var size = 0L
    private var tail: Thread? = null
    private val stamp = SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss.SSS", Locale.US)

    /** Starts the journal for this process. Harmless to call again. */
    @Synchronized
    fun init(context: Context) {
        if (file != null) return
        val f = File(context.filesDir, FILE)
        file = f
        size = if (f.exists()) f.length() else 0
        // A file far over the limit was not written by this code.
        if (size > MAX_BYTES * 2) {
            f.delete()
            size = 0
        } else if (size >= MAX_BYTES) {
            rotate()
        }
        catchCrashes()
        startTail()
        write("android", 'I', "process started")
    }

    /** A line from the app's own Kotlin code. */
    fun i(message: String) = write("android", 'I', message, echo = true)

    fun e(message: String, error: Throwable? = null) = write(
        "android",
        'E',
        if (error == null) message else "$message: ${Log.getStackTraceString(error)}",
        echo = true,
    )

    @Synchronized
    private fun write(
        source: String,
        level: Char,
        message: String,
        millis: Long = System.currentTimeMillis(),
        echo: Boolean = false,
    ) {
        val clean = Scrub.line(message)
        if (echo) {
            Log.println(if (level == 'E') Log.ERROR else Log.INFO, TAG, clean)
        }
        val f = file ?: return
        // The zone is looked up for every line: the phone may have moved to another
        // since the process started (it does at boot, and when travelling), and the
        // Dart side's journal follows it.
        stamp.timeZone = TimeZone.getDefault()
        val entry = "${stamp.format(Date(millis))} [$source $level] $clean\n"
        val bytes = entry.toByteArray(Charsets.UTF_8)
        try {
            if (size > 0 && size + bytes.size > MAX_BYTES) rotate()
            f.appendBytes(bytes)
            size += bytes.size
        } catch (e: Exception) {
            // The journal is a help; losing a line is not worth stopping for.
        }
    }

    private fun rotate() {
        val f = file ?: return
        val old = File(f.path + ".1")
        old.delete()
        f.renameTo(old)
        size = 0
    }

    /** A crash of the process ends up in the journal before the process goes. */
    private fun catchCrashes() {
        val previous = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { thread, error ->
            try {
                write("android", 'E', "crash in ${thread.name}: ${Log.getStackTraceString(error)}")
            } catch (ignored: Throwable) {
            }
            previous?.uncaughtException(thread, error)
        }
    }

    // 1763112605.123  1234  5678 D awg     : peer(AbCd…WxYz) - Sending handshake initiation
    // The time is in seconds since 1970 (-v epoch): a logcat that keeps running
    // would otherwise go on printing in the zone it was started in.
    private val logcatLine =
        Regex("""^\s*(\d+)\.(\d{3})\s+\d+\s+\d+ ([VDIWEF]) ([^:]*?)\s*: ?(.*)$""")

    /**
     * Reads the tunnel library's lines from the system log as they come, from
     * now on (what came before was copied by the run before, if there was one).
     * If the reader stops (the system restarts its log service), it is started
     * again from where it left off.
     */
    private fun startTail() {
        if (tail != null) return
        tail = Thread({
            var since = epoch(System.currentTimeMillis())
            var wait = 5_000L
            while (true) {
                var read = 0
                try {
                    val process = ProcessBuilder(
                        "logcat", "-v", "threadtime", "-v", "epoch", "-T", since, "awg:V", "*:S",
                    ).redirectErrorStream(true).start()
                    BufferedReader(InputStreamReader(process.inputStream, Charsets.UTF_8)).use { r ->
                        while (true) {
                            val line = r.readLine() ?: break
                            val m = logcatLine.find(line) ?: continue
                            val (seconds, ms, level, _, text) = m.destructured
                            since = "$seconds.$ms"
                            read++
                            write("awg", level[0], text, millis = seconds.toLong() * 1000 + ms.toLong())
                        }
                    }
                    process.destroy()
                } catch (e: Exception) {
                    if (wait == 5_000L) write("android", 'E', "could not read the tunnel library's log: $e")
                }
                // A reader that keeps failing is tried less and less often.
                wait = if (read > 0) 5_000L else minOf(wait * 2, 300_000L)
                Thread.sleep(wait)
            }
        }, "awg-log").apply {
            isDaemon = true
            start()
        }
    }

    /** "1763112605.123": a moment as logcat's -T takes it. */
    private fun epoch(millis: Long) = "${millis / 1000}.${(millis % 1000).toString().padStart(3, '0')}"
}

/**
 * What must not reach the journal: keys and the public addresses of servers.
 * The Dart side scrubs everything again, more thoroughly, when the journal is
 * saved (lib/core/log_scrub.dart); this is so that the file on the phone is
 * clean too. The addresses inside the tunnel and on the home network are kept.
 */
object Scrub {
    private val keyLine =
        Regex("""\b(PrivateKey|PresharedKey|PublicKey)(\s*[=:]\s*)\S+""", RegexOption.IGNORE_CASE)
    private val wgKey = Regex("""(?<![A-Za-z0-9+/])[A-Za-z0-9+/]{42,43}=""")
    private val ipv4 = Regex("""(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?![\d.]*\d)""")

    fun line(s: String): String {
        var out = keyLine.replace(s) { "${it.groupValues[1]}${it.groupValues[2]}‹hidden›" }
        out = wgKey.replace(out, "‹key›")
        out = ipv4.replace(out) { m ->
            val p = m.groupValues.drop(1).map { it.toInt() }
            if (p.any { it > 255 }) return@replace m.value
            val private = p[0] == 10 || p[0] == 127 || p[0] == 0 || p[0] >= 224 ||
                (p[0] == 172 && p[1] in 16..31) || (p[0] == 192 && p[1] == 168) ||
                (p[0] == 169 && p[1] == 254) || (p[0] == 100 && p[1] in 64..127) ||
                (p[0] == 198 && p[1] in 18..19)
            if (private) m.value else "${p[0]}.x.x.${p[3]}"
        }
        return out
    }
}
