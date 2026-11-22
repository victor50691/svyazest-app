package com.svyazest.svyazest_app

import android.content.Context
import android.net.Network
import java.io.File
import java.util.concurrent.TimeUnit

/**
 * Xray-core 26.10.31 next to the embedded 26.3.27 (1.17.0, 2026-11-21).
 *
 * Keys whose JSON (XHTTP `extra`, Hysteria2 `fm`) carries fields only 26.10
 * knows are dialed by it -- the server decides (executor_jobs.needs_core,
 * telegram-bot/services/xray_cores.py, same rule as DPI//Checker's xray-next).
 * Two Go runtimes cannot live in one process, so this one is a separate
 * executable: libxraynext.so (a real ELF, extracted to nativeLibraryDir like
 * the old libxray.so), built from upstream source plus one file,
 * xraynext/zz_svyazest_android.go, that pins the whole process to the
 * cellular network (android_setprocnetwork, handle in SVZ_NET_HANDLE) before
 * anything dials, and exits (code 97) if it can't -- never leaks through a VPN.
 */
object XrayNext {
    private const val PIN_FAILED = 97

    @Volatile private var proc: Process? = null
    @Volatile private var cfgFile: File? = null
    @Volatile private var lastError: String? = null
    @Volatile private var pinnedOk = false
    private val tail = ArrayDeque<String>()
    @Volatile private var version: String? = null

    private fun bin(ctx: Context) = File(ctx.applicationInfo.nativeLibraryDir, "libxraynext.so")

    /** null = started and listening soon; otherwise why not. */
    @Synchronized
    fun start(ctx: Context, config: String, net: Network?): String? {
        stopQuietly()
        lastError = null
        pinnedOk = false
        val b = bin(ctx)
        if (!b.exists()) return "xray 26.10 не найден в приложении".also { lastError = it }
        if (net == null) return "no cellular network".also { lastError = it }
        val f = File(ctx.cacheDir, "xray-next-${System.nanoTime()}.json")
        f.writeText(config)
        cfgFile = f
        synchronized(tail) { tail.clear() }
        val p = try {
            ProcessBuilder(b.absolutePath, "run", "-c", f.absolutePath)
                .redirectErrorStream(true)
                .also {
                    it.environment()["SVZ_NET_HANDLE"] = net.networkHandle.toString()
                    it.environment()["XRAY_LOCATION_ASSET"] = ctx.cacheDir.absolutePath
                }
                .start()
        } catch (e: Exception) {
            f.delete()
            return "xray 26.10: ${e.javaClass.simpleName}".also { lastError = it }
        }
        proc = p
        // Drain the output (a full pipe would block the core); keep the last
        // lines only for an error message.
        Thread {
            try {
                p.inputStream.bufferedReader().forEachLine { line ->
                    synchronized(tail) { tail.addLast(line.take(200)); while (tail.size > 5) tail.removeFirst() }
                }
            } catch (_: Exception) {}
        }.apply { isDaemon = true }.start()
        // A bad config or a failed network pin ends the process at once.
        if (p.waitFor(700, TimeUnit.MILLISECONDS)) {
            val code = p.exitValue()
            proc = null
            f.delete()
            cfgFile = null
            val last = synchronized(tail) { tail.lastOrNull() }.orEmpty()
            val msg = if (code == PIN_FAILED) "could not pin to the cellular network" else "xray 26.10 exited ($code) $last".trim()
            NativeLog.add("xray", "ядро 26.10 не запустилось: код $code")
            return msg.also { lastError = it }
        }
        pinnedOk = true
        NativeLog.add("xray", "ядро 26.10 запущено отдельным процессом, привязано к мобильной сети")
        return null
    }

    fun isRunning() = proc != null

    /** Same shape as the embedded core's stats: the whole process is pinned,
     *  so "bound" means the pin held for this run. */
    @Synchronized
    fun stop(): Map<String, Any?> {
        val wasPinned = pinnedOk
        val err = lastError
        stopQuietly()
        NativeLog.add("xray", "ядро 26.10 остановлено")
        return mapOf("bound" to (if (wasPinned) 1 else 0), "unbound" to (if (wasPinned || err == null) 0 else 1), "error" to err, "core" to "26.10")
    }

    private fun stopQuietly() {
        proc?.let { p ->
            p.destroy()
            if (!p.waitFor(1, TimeUnit.SECONDS)) p.destroyForcibly()
        }
        proc = null
        cfgFile?.delete()
        cfgFile = null
        pinnedOk = false
    }

    /** "26.10.31" read from the binary once ("" when it can't run). */
    fun version(ctx: Context): String {
        version?.let { return it }
        val v = try {
            val p = ProcessBuilder(bin(ctx).absolutePath, "version").redirectErrorStream(true).start()
            val out = p.inputStream.bufferedReader().readText()
            p.waitFor(3, TimeUnit.SECONDS)
            Regex("""Xray\s+v?(\d+\.\d+\.\d+)""").find(out)?.groupValues?.get(1).orEmpty()
        } catch (_: Exception) { "" }
        if (v.isNotEmpty()) version = v
        return v
    }
}
