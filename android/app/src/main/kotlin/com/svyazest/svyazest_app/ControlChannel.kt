package com.svyazest.svyazest_app

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.net.HttpURLConnection
import java.net.InetSocketAddress
import java.net.Proxy
import java.net.Socket
import java.net.URL

/**
 * The app's link to its own server ("control channel"), separate from the
 * checks (1.11.0, 2026-11-21).
 *
 * Checks must go over the bare cellular network -- that is what they
 * measure, so the whole process stays bound to it (bindProcessToNetwork).
 * Under «белые списки» our server is usually NOT reachable over that same
 * network, and the partner wants to keep using the internet through a VPN
 * anyway. So every request to the server picks its own route, first that
 * works:
 *
 *  1. the VPN itself -- only possible when the app is inside a VPN that
 *     allows apps to go around it (most don't; then the app has to be in
 *     the VPN's exclusions and Android refuses the VPN to it: EPERM);
 *  2. Wi-Fi (or ethernet) if connected;
 *  3. the local proxy of the VPN client (v2rayNG, Hiddify, NekoBox, Happ…
 *     keep a SOCKS5/HTTP proxy on 127.0.0.1) -- found automatically on the
 *     usual ports, or the port the partner entered in Настройки;
 *  4. the cellular network (no VPN, no Wi-Fi, no whitelist: as before).
 *
 * The working route is remembered; it is tried first next time and
 * re-checked on failure. A route is only abandoned on a connect-phase error
 * (nothing was sent), so a POST is never delivered twice.
 *
 * 1.21.1 (2026-11-24, partner on Wi-Fi that does not reach the server):
 *  - TCP + TLS must finish within [CONNECT_DEADLINE_MS], else the attempt is
 *    cut (a hung TLS handshake used to wait the 70 s read timeout while the
 *    Dart side gave up after 15 s, so every request kept going to Wi-Fi);
 *  - a route that broke after connecting is also put aside, and a GET (no
 *    body, safe to repeat) moves on to the next route at once;
 *  - a route that keeps failing is put aside longer (1 → 2 → 4 → 8 → 10 min),
 *    a different Wi-Fi network starts afresh;
 *  - Wi-Fi that Android has not validated (no internet / login page) goes
 *    after the VPN client's proxy.
 */
object ControlChannel {
    private const val PREFS = "svyazest_control"
    private val COMMON_PROXY_PORTS = intArrayOf(10808, 10809, 10810, 2080, 2081, 12334, 7890, 7891, 1080, 1081, 10801, 20170, 20808, 3067, 8080, 9050)

    data class Route(val id: String, val label: String, val network: Network?, val proxy: Proxy?, val short: String = label)

    @Volatile private var lastGood: String? = null
    @Volatile var lastRouteLabel: String = "—"
    // Short form for Настройки («Сейчас через»).
    @Volatile var lastRouteShort: String = "—"
    @Volatile var lastError: String? = null
    @Volatile private var proxyCache: Pair<Long, Route?>? = null
    // Route id -> when it last failed to connect: such a route goes to the
    // end of the list for a minute instead of being retried first (1.14.2;
    // before, the last good route was always tried first, so once the
    // cellular network had worked the phone never went back to the VPN
    // client's proxy even though it comes earlier in the order).
    private val failedAt = java.util.concurrent.ConcurrentHashMap<String, Long>()
    // 1.21.1: failures in a row per route (longer pause each time) and the
    // network it failed on (another Wi-Fi network is not punished for it).
    private val failCount = java.util.concurrent.ConcurrentHashMap<String, Int>()
    private val failedNet = java.util.concurrent.ConcurrentHashMap<String, Long>()
    private const val CONNECT_DEADLINE_MS = 6000L
    private val watchdog = java.util.concurrent.Executors.newSingleThreadScheduledExecutor { r ->
        Thread(r, "control-watchdog").apply { isDaemon = true }
    }

    private fun penaltyMs(id: String): Long {
        val n = (failCount[id] ?: 1).coerceIn(1, 5)
        return minOf(60_000L shl (n - 1), 600_000L)
    }

    private fun markFailed(r: Route) {
        val net = r.network?.networkHandle ?: 0L
        val same = failedNet[r.id] == net
        failCount[r.id] = if (same) (failCount[r.id] ?: 0) + 1 else 1
        failedNet[r.id] = net
        failedAt[r.id] = System.currentTimeMillis()
        if (r.id == lastGood) lastGood = null
        if (r.proxy != null) proxyCache = null
    }

    private fun markGood(r: Route) {
        failedAt.remove(r.id)
        failCount.remove(r.id)
        failedNet.remove(r.id)
    }

    private fun penalized(r: Route, now: Long): Boolean {
        val f = failedAt[r.id] ?: return false
        if (r.network != null && failedNet[r.id] != r.network.networkHandle) return false
        return now - f < penaltyMs(r.id)
    }

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
    fun manualPort(ctx: Context): Int = prefs(ctx).getInt("proxy_port", 0)
    fun setManualPort(ctx: Context, port: Int) {
        prefs(ctx).edit().putInt("proxy_port", if (port in 1..65535) port else 0).apply()
        proxyCache = null
        lastGood = null
    }

    // Login/password of the VPN client's proxy (1.16.2, Настройки → «Прокси
    // VPN-клиента»); empty = no auth. Private app storage, no backup.
    @Volatile private var creds: Pair<String, String>? = null

    private fun loadCreds(ctx: Context): Pair<String, String>? {
        val p = prefs(ctx)
        val u = p.getString("proxy_user", "").orEmpty()
        return if (u.isEmpty()) null else u to p.getString("proxy_pass", "").orEmpty()
    }

    fun setProxy(ctx: Context, port: Int, user: String, pass: String) {
        // "\u0000keep": same login, password field left empty -> keep the stored one.
        val keep = pass == "\u0000keep"
        val newPass = when {
            user.trim().isEmpty() -> ""
            keep -> prefs(ctx).getString("proxy_pass", "").orEmpty()
            else -> pass
        }
        prefs(ctx).edit()
            .putInt("proxy_port", if (port in 1..65535) port else 0)
            .putString("proxy_user", user.trim())
            .putString("proxy_pass", newPass)
            .apply()
        creds = loadCreds(ctx)
        installAuthenticator()
        proxyCache = null
        lastGood = null
    }

    /** SOCKS5 (java.net SocksSocketImpl) and HTTP proxies (HttpURLConnection,
     *  407) ask java.net.Authenticator for the login -- only ever given to a
     *  proxy on this phone (127.0.0.1). */
    @Volatile private var authInstalled = false
    private fun installAuthenticator() {
        if (authInstalled) return
        authInstalled = true
        java.net.Authenticator.setDefault(object : java.net.Authenticator() {
            override fun getPasswordAuthentication(): java.net.PasswordAuthentication? {
                val c = creds ?: return null
                val host = requestingHost ?: requestingSite?.hostAddress
                if (host != "127.0.0.1" && host != "localhost" && requestingSite?.isLoopbackAddress != true) return null
                return java.net.PasswordAuthentication(c.first, c.second.toCharArray())
            }
        })
    }

    private fun ensureCreds(ctx: Context) {
        if (creds == null) creds = loadCreds(ctx)
        if (creds != null) installAuthenticator()
    }

    private fun cm(ctx: Context) = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager

    private fun networks(ctx: Context, test: (NetworkCapabilities) -> Boolean): List<Network> {
        val cm = cm(ctx)
        val all: Array<Network> = try { cm.allNetworks } catch (_: Exception) { emptyArray() }
        return all.filter { n ->
            val c = cm.getNetworkCapabilities(n) ?: return@filter false
            c.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) && test(c)
        }
    }

    private fun readFully(s: Socket, n: Int): ByteArray? {
        val b = ByteArray(n)
        var got = 0
        while (got < n) { val r = s.getInputStream().read(b, got, n - got); if (r < 0) return null; got += r }
        return b
    }

    /** What listens on 127.0.0.1:[port], in words, and whether it is a usable proxy. */
    private fun probePort(port: Int, host: String): Pair<String, Proxy.Type?> {
        val c = creds
        // SOCKS5: offer "no auth" (and "login/password" when one is set) ->
        // 05 00 usable; 05 02 -> RFC 1929 login, 01 00 = accepted.
        try {
            Socket().use { s ->
                s.connect(InetSocketAddress("127.0.0.1", port), 300)
                s.soTimeout = 800
                s.getOutputStream().apply { write(if (c != null) byteArrayOf(5, 2, 0, 2) else byteArrayOf(5, 1, 0)); flush() }
                val b = readFully(s, 2)
                if (b != null && b[0] == 5.toByte() && b[1] == 0.toByte()) return "SOCKS5 — подходит" to Proxy.Type.SOCKS
                if (b != null && b[0] == 5.toByte() && b[1] == 2.toByte() && c != null) {
                    val u = c.first.toByteArray(Charsets.UTF_8).take(255).toByteArray()
                    val p = c.second.toByteArray(Charsets.UTF_8).take(255).toByteArray()
                    s.getOutputStream().apply { write(byteArrayOf(1, u.size.toByte()) + u + byteArrayOf(p.size.toByte()) + p); flush() }
                    val a = readFully(s, 2)
                    return if (a != null && a[1] == 0.toByte()) "SOCKS5 с логином — подходит" to Proxy.Type.SOCKS
                    else "SOCKS5: логин или пароль не подошли" to null
                }
                if (b != null && b[0] == 5.toByte()) return (if (c == null) "SOCKS5 просит логин и пароль — укажите их в «Прокси VPN-клиента»" else "SOCKS5 не принял способ входа") to null
            }
        } catch (e: java.net.ConnectException) {
            return "закрыт" to null
        } catch (e: java.net.SocketTimeoutException) {
            // open but silent to SOCKS -- maybe an HTTP proxy, checked below
        } catch (e: Exception) {
            return "ошибка: ${e.javaClass.simpleName}" to null
        }
        try {
            Socket().use { s ->
                s.connect(InetSocketAddress("127.0.0.1", port), 300)
                s.soTimeout = 3000
                val auth = c?.let {
                    "Proxy-Authorization: Basic " + android.util.Base64.encodeToString("${it.first}:${it.second}".toByteArray(Charsets.UTF_8), android.util.Base64.NO_WRAP) + "\r\n"
                } ?: ""
                s.getOutputStream().apply { write("CONNECT $host:443 HTTP/1.1\r\nHost: $host:443\r\n$auth\r\n".toByteArray()); flush() }
                val buf = ByteArray(64)
                val n = s.getInputStream().read(buf)
                val head = if (n > 0) String(buf, 0, n) else ""
                if (head.startsWith("HTTP/1.") && head.length >= 12 && head.substring(9, 12) == "200") return "HTTP-прокси — подходит" to Proxy.Type.HTTP
                if (head.startsWith("HTTP/1.") && head.length >= 12 && head.substring(9, 12) == "407") {
                    return (if (c == null) "HTTP-прокси просит логин и пароль — укажите их в «Прокси VPN-клиента»" else "HTTP-прокси: логин или пароль не подошли") to null
                }
                return "открыт, но это не прокси" to null
            }
        } catch (e: java.net.ConnectException) {
            return "закрыт" to null
        } catch (e: Exception) {
            return "открыт, но это не прокси" to null
        }
    }

    private fun ports(ctx: Context) = (listOf(manualPort(ctx)).filter { it > 0 } + COMMON_PROXY_PORTS.toList()).distinct()

    /** Looks for the VPN client's local proxy: the partner's port first, then the usual ones. */
    private fun scan(ctx: Context, host: String): Pair<Route?, List<String>> {
        val ports = ports(ctx)
        val report = mutableListOf<String>()
        var found: Route? = null
        for (p in ports) {
            val (what, type) = probePort(p, host)
            if (what != "закрыт") report.add("$p: $what")
            if (type != null && found == null) {
                val socks = type == Proxy.Type.SOCKS
                found = Route("${if (socks) "socks" else "http"}:$p",
                    "прокси VPN-клиента (${if (socks) "SOCKS5" else "HTTP"}, порт $p)", null,
                    Proxy(type, InetSocketAddress("127.0.0.1", p)), "прокси VPN · $p")
            }
        }
        NativeLog.add("связь", "поиск прокси VPN-клиента на 127.0.0.1, порты ${ports.joinToString(",")}: " +
            (if (report.isEmpty()) "все закрыты" else report.joinToString("; ")) + (found?.let { " → берём ${it.short}" } ?: " → прокси нет"))
        return found to report
    }

    /** Cached 60 s. */
    private fun localProxy(ctx: Context, host: String): Route? {
        ensureCreds(ctx)
        val c = proxyCache
        if (c != null && System.currentTimeMillis() - c.first < 60_000) return c.second
        val found = scan(ctx, host).first
        proxyCache = System.currentTimeMillis() to found
        return found
    }

    /** Настройки → «Найти прокси VPN-клиента»: a fresh scan with the per-port report. */
    fun scanNow(ctx: Context): Map<String, Any?> {
        val (found, report) = scan(ctx, "dpichecker.st")
        proxyCache = System.currentTimeMillis() to found
        found?.let { failedAt.remove(it.id) }
        return mapOf("found" to found?.short, "report" to report, "ports" to ports(ctx).joinToString(", "))
    }

    private fun routes(ctx: Context, host: String): List<Route> {
        val list = mutableListOf<Route>()
        networks(ctx) { it.hasTransport(NetworkCapabilities.TRANSPORT_VPN) }.firstOrNull()
            ?.let { list.add(Route("vpn", "VPN", it, null)) }
        val cmgr = cm(ctx)
        val wifi = networks(ctx) {
            !it.hasTransport(NetworkCapabilities.TRANSPORT_VPN) && !it.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)
        }.sortedByDescending { n ->
            cmgr.getNetworkCapabilities(n)?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true
        }.firstOrNull()
        val wifiValidated = wifi != null &&
            cmgr.getNetworkCapabilities(wifi)?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true
        val wifiRoute = wifi?.let { Route("wifi", if (wifiValidated) "Wi-Fi" else "Wi-Fi (без проверки интернета)", it, null, "Wi-Fi") }
        if (wifiRoute != null && wifiValidated) list.add(wifiRoute)
        localProxy(ctx, host)?.let { list.add(it) }
        if (wifiRoute != null && !wifiValidated) list.add(wifiRoute)
        networks(ctx) {
            it.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) && !it.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
        }.firstOrNull()?.let { list.add(Route("cellular", "мобильная сеть", it, null, "мобильная сеть")) }
        // The order above, except routes that failed recently go last.
        val now = System.currentTimeMillis()
        return list.sortedBy { r -> if (penalized(r, now)) 1 else 0 }
    }

    /**
     * One HTTP(S) request over the first route that connects. Blocking --
     * call off the main thread. Result: status (0 = not sent), headers,
     * body bytes, the route used, error text.
     */
    fun request(ctx: Context, method: String, url: String, headers: Map<String, String>, body: ByteArray?,
                readTimeoutMs: Int, toFile: java.io.File? = null): Map<String, Any?> {
        val u = URL(url)
        // The log upload itself is not logged (every upload would add a line).
        val quiet = u.path.contains("/diag/")
        val tried = mutableListOf<String>()
        var lastErr: String? = null
        for (r in routes(ctx, u.host)) {
            var conn: HttpURLConnection? = null
            try {
                conn = (when {
                    r.network != null -> r.network.openConnection(u)
                    r.proxy != null -> u.openConnection(r.proxy)
                    else -> u.openConnection()
                }) as HttpURLConnection
                conn.requestMethod = method
                conn.connectTimeout = 8000
                // 1.21.1: the Dart side gives up after 40 s on /jobs/poll (server
                // holds it 25 s) and after ≤30 s on everything else -- waiting
                // longer here only kept a hung route in front of the others.
                conn.readTimeout = if (toFile != null) readTimeoutMs
                    else minOf(readTimeoutMs, if (u.path.endsWith("/jobs/poll")) 35_000 else 30_000)
                conn.instanceFollowRedirects = false
                conn.useCaches = false
                for ((k, v) in headers) conn.setRequestProperty(k, v)
                // Which way this request went (vpn / wifi / socks:10808 / cellular): the
                // server records it next to the result (partner_network.request_route).
                conn.setRequestProperty("X-Net-Route", r.id)
                if (body != null && method != "GET") conn.doOutput = true
                val t0 = System.currentTimeMillis()
                // TCP + TLS within CONNECT_DEADLINE_MS: past it the attempt is
                // cut from here (disconnect() closes the socket being set up)
                // and the next route is tried.
                val cut = java.util.concurrent.atomic.AtomicBoolean(false)
                val c0: HttpURLConnection = conn
                val guard = watchdog.schedule({ cut.set(true); runCatching { c0.disconnect() } },
                    CONNECT_DEADLINE_MS, java.util.concurrent.TimeUnit.MILLISECONDS)
                try {
                    conn.connect() // TCP + TLS: nothing of the request has left yet
                    guard.cancel(false)
                    if (cut.get()) throw IOException("не подключился за ${CONNECT_DEADLINE_MS / 1000} с")
                } catch (e: Exception) {
                    guard.cancel(false)
                    val why = if (cut.get()) "не подключился за ${CONNECT_DEADLINE_MS / 1000} с (TCP/TLS)" else describe(e)
                    lastErr = "${r.id}: $why"
                    tried.add(lastErr)
                    markFailed(r)
                    NativeLog.add("связь", "${method} ${u.path}: путь «${r.label}» не подключился за ${System.currentTimeMillis() - t0} мс — $why; отложен на ${penaltyMs(r.id) / 1000} с")
                    continue
                }
                if (body != null && conn.doOutput) conn.outputStream.use { it.write(body) }
                val code = conn.responseCode
                val stream = if (code >= 400) conn.errorStream else conn.inputStream
                // In-app update (1.19.0): a 200 body goes straight to a file
                // (an APK is ~50 MB -- not through memory / the method channel).
                var saved = 0L
                if (toFile != null && code == 200 && stream != null) {
                    toFile.parentFile?.mkdirs()
                    stream.use { s -> toFile.outputStream().use { o ->
                        val buf = ByteArray(64 * 1024)
                        while (true) { val n = s.read(buf); if (n < 0) break; o.write(buf, 0, n); saved += n }
                    } }
                }
                val bytes = if (toFile != null && code == 200) ByteArray(0) else stream?.use { s ->
                    val out = ByteArrayOutputStream()
                    val buf = ByteArray(16 * 1024)
                    while (true) { val n = s.read(buf); if (n < 0) break; out.write(buf, 0, n) }
                    out.toByteArray()
                } ?: ByteArray(0)
                val hdrs = mutableMapOf<String, String>()
                for ((k, v) in conn.headerFields) if (k != null && v != null) hdrs[k.lowercase()] = v.joinToString(", ")
                if (lastGood != r.id) NativeLog.add("связь", "путь к серверу теперь: ${r.label}")
                if (!quiet) NativeLog.add("связь", "${method} ${u.path} → HTTP $code за ${System.currentTimeMillis() - t0} мс через «${r.label}», ${body?.size ?: 0}/${bytes.size} Б")
                lastGood = r.id
                markGood(r)
                lastRouteLabel = r.label
                lastRouteShort = r.short
                lastError = null
                return mapOf("status" to code, "headers" to hdrs, "body" to bytes, "route" to r.label, "error" to null,
                    "failed" to tried.joinToString("; "), "saved" to saved)
            } catch (e: Exception) {
                // After connect. The route is put aside either way, so the next
                // requests take another one (before 1.21.1 a route that connected
                // but then hung stayed first forever).
                markFailed(r)
                lastErr = "${r.id}: обрыв — ${describe(e)}"
                tried.add(lastErr)
                // A GET carries nothing and changes nothing: repeat it on the
                // next route at once. A POST may have reached the server -- no
                // retry elsewhere (the caller decides).
                if (method == "GET" && toFile == null) {
                    NativeLog.add("связь", "${method} ${u.path}: обрыв после подключения через «${r.label}» — ${describe(e)}; повтор следующим путём, этот отложен на ${penaltyMs(r.id) / 1000} с")
                    continue
                }
                lastError = "${r.id}: ${describe(e)}"
                NativeLog.add("связь", "${method} ${u.path}: обрыв после подключения через «${r.label}» — ${describe(e)}; путь отложен на ${penaltyMs(r.id) / 1000} с")
                return mapOf("status" to 0, "headers" to emptyMap<String, String>(), "body" to ByteArray(0),
                    "route" to r.label, "error" to describe(e))
            } finally {
                conn?.disconnect()
            }
        }
        lastRouteLabel = "нет связи"
        lastRouteShort = "нет связи"
        lastError = tried.joinToString("; ").ifEmpty { "нет ни одной сети" }
        NativeLog.add("связь", "${method} ${u.path}: НЕТ СВЯЗИ с сервером ни одним путём ($lastError)")
        return mapOf("status" to 0, "headers" to emptyMap<String, String>(), "body" to ByteArray(0), "route" to null,
            "error" to "network is unreachable: ${lastError}")
    }

    /** Words the Dart side already understands as "plain network trouble". */
    private fun describe(e: Exception): String = when (e) {
        is java.net.SocketTimeoutException -> "timed out (${e.message ?: ""})"
        is java.net.UnknownHostException -> "Failed host lookup: ${e.message ?: ""}"
        else -> (e.javaClass.simpleName + ": " + (e.message ?: "")).take(200)
    }

    fun info(ctx: Context): Map<String, Any?> = mapOf(
        "route" to lastRouteLabel, "route_short" to lastRouteShort, "error" to lastError, "manual_port" to manualPort(ctx),
        "proxy_user" to prefs(ctx).getString("proxy_user", "").orEmpty(),
    )
}
