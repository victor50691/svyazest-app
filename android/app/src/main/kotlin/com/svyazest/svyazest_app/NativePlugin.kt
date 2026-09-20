package com.svyazest.svyazest_app

import android.content.Context
import android.content.Intent
import android.net.Uri
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Build
import android.os.ParcelFileDescriptor
import android.media.MediaDrm
import android.provider.Settings
import android.os.PowerManager
import android.net.TrafficStats
import android.telephony.TelephonyManager
import java.util.concurrent.atomic.AtomicInteger
import androidx.core.app.NotificationManagerCompat
import java.security.MessageDigest
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.spec.ECGenParameterSpec
import java.util.UUID
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import android.system.Os
import android.system.OsConstants
import android.os.Handler
import android.os.Looper
import io.flutter.embedding.engine.plugins.FlutterPlugin
import io.flutter.plugin.common.EventChannel
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.io.File

/**
 * Everything Dart genuinely cannot do on its own, packaged as a real
 * FlutterPlugin so it is attached to EVERY engine: the UI one (from
 * MainActivity) and the foreground service's own engine (from App.kt via
 * flutter_foreground_task's lifecycle listener). The previous MainActivity-
 * only MethodChannel silently did not exist in the service engine, so the
 * job loop failed on its very first native call every cycle.
 */
class NativePlugin : FlutterPlugin, MethodChannel.MethodCallHandler, EventChannel.StreamHandler {
    companion object {
        const val METHOD_CHANNEL = "com.svyazest.svyazest_app/native"
        const val EVENT_CHANNEL = "com.svyazest.svyazest_app/network"
    }

    private lateinit var context: Context
    private var methodChannel: MethodChannel? = null
    private var eventChannel: EventChannel? = null
    private var networkCallback: ConnectivityManager.NetworkCallback? = null
    private val mainHandler = Handler(Looper.getMainLooper())

    override fun onAttachedToEngine(binding: FlutterPlugin.FlutterPluginBinding) {
        context = binding.applicationContext
        methodChannel = MethodChannel(binding.binaryMessenger, METHOD_CHANNEL).also { it.setMethodCallHandler(this) }
        eventChannel = EventChannel(binding.binaryMessenger, EVENT_CHANNEL).also { it.setStreamHandler(this) }
    }

    override fun onDetachedFromEngine(binding: FlutterPlugin.FlutterPluginBinding) {
        methodChannel?.setMethodCallHandler(null)
        eventChannel?.setStreamHandler(null)
        stopWatching()
        releaseCellularRequest()
    }

    override fun onMethodCall(call: MethodCall, result: MethodChannel.Result) {
        when (call.method) {
            // Path to this app's own native-library directory, where
            // libxray.so (a renamed real ELF binary, see
            // third_party/xray-core/README.md) lives once installed.
            "nativeLibraryDir" -> result.success(context.applicationInfo.nativeLibraryDir)
            "hasMobileData" -> result.success(hasMobileData())
            "hasActiveVpn" -> result.success(hasActiveVpn())
            "networkState" -> result.success(networkState())
            "isRooted" -> result.success(isRooted())
            "hwid" -> result.success(hwid())
            "openNotificationSettings" -> result.success(openNotificationSettings())
            // Both work from the application context, i.e. also inside the
            // foreground service where flutter_foreground_task's own
            // checkNotificationPermission() fails for want of an Activity.
            "permissions" -> result.success(mapOf(
                "notifications" to NotificationManagerCompat.from(context).areNotificationsEnabled(),
                "battery" to runCatching {
                    (context.getSystemService(Context.POWER_SERVICE) as PowerManager)
                        .isIgnoringBatteryOptimizations(context.packageName)
                }.getOrDefault(true),
            ))
            "attest" -> {
                val challenge = call.argument<String>("challenge") ?: ""
                // Key generation in the secure hardware takes up to a second
                // or two -- keep it off the main thread.
                Thread {
                    val reply: Map<String, Any?> = try {
                        mapOf("chain" to attest(Base64.decode(challenge, Base64.NO_WRAP)))
                    } catch (e: Throwable) {
                        mapOf("error" to (e.javaClass.simpleName + ": " + (e.message ?: "")).take(200))
                    }
                    mainHandler.post { result.success(reply) }
                }.start()
            }

            // ---- cellular pinning (see "cellular" section below) ----
            "cellularRequest" -> {
                if (call.argument<Boolean>("enable") == true) ensureCellularRequest() else releaseCellularRequest()
                result.success(findCellular() != null)
            }
            "bindProcessToCellular" -> {
                if (call.argument<Boolean>("enable") == true) {
                    // The cellular link can take a moment to come up after
                    // requestNetwork() (Wi-Fi phones keep it down otherwise):
                    // wait for it off the main thread instead of reporting
                    // "not bound" on the first cycle.
                    val waitMs = (call.argument<Int>("waitMs") ?: 0).toLong()
                    Thread {
                        val net = waitForCellular(waitMs)
                        var error: String? = null
                        val ok = if (net == null) {
                            error = "no cellular network"
                            false
                        } else try {
                            cm().bindProcessToNetwork(net).also { if (!it) error = "bindProcessToNetwork returned false" }
                        } catch (e: Exception) {
                            error = e.javaClass.simpleName + ": " + (e.message ?: "")
                            false
                        }
                        lastBindError = error
                        mainHandler.post { result.success(mapOf("ok" to ok, "cellular" to (net != null), "error" to error)) }
                    }.start()
                } else {
                    runCatching { cm().bindProcessToNetwork(null) }
                    result.success(true)
                }
            }
            "operatorName" -> result.success(operatorName())
            "whitelistProbe" -> {
                val allowed = call.argument<List<String>>("allowed") ?: emptyList()
                val control = call.argument<List<String>>("control") ?: emptyList()
                val timeoutMs = call.argument<Int>("timeoutMs") ?: 3000
                Thread {
                    val reply = whitelistProbe(allowed, control, timeoutMs)
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            "hostProbe" -> {
                val host = call.argument<String>("host") ?: ""
                val timeoutMs = call.argument<Int>("timeoutMs") ?: 5000
                Thread {
                    val reply = hostProbe(host, timeoutMs)
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            "subnetProbe" -> {
                val cidr = call.argument<String>("cidr") ?: ""
                val timeoutMs = call.argument<Int>("timeoutMs") ?: 1200
                val concurrency = call.argument<Int>("concurrency") ?: 32
                Thread {
                    val reply = subnetProbe(cidr, timeoutMs, concurrency)
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            "cellularPost" -> {
                val url = call.argument<String>("url") ?: ""
                val body = call.argument<String>("body") ?: ""
                val token = call.argument<String>("token")
                val waitMs = (call.argument<Int>("waitMs") ?: 0).toLong()
                Thread {
                    val reply = cellularPost(url, body, token, waitMs)
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            "networkCountry" -> result.success(runCatching {
                val tm = context.getSystemService(Context.TELEPHONY_SERVICE) as TelephonyManager
                mapOf(
                    "network" to tm.networkCountryIso?.lowercase()?.takeIf { it.length == 2 },
                    "sim" to tm.simCountryIso?.lowercase()?.takeIf { it.length == 2 },
                )
            }.getOrDefault(mapOf("network" to null, "sim" to null)))
            "vpnLockdown" -> {
                Thread {
                    val r = vpnLockdown()
                    mainHandler.post { result.success(r) }
                }.start()
            }
            "openVpnSettings" -> result.success(runCatching {
                context.startActivity(Intent(Settings.ACTION_VPN_SETTINGS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                true
            }.getOrDefault(false))
            "uidTraffic" -> result.success(mapOf(
                "rx" to TrafficStats.getUidRxBytes(android.os.Process.myUid()),
                "tx" to TrafficStats.getUidTxBytes(android.os.Process.myUid()),
            ))

            // ---- embedded Xray-core (xraylib.aar, see ../../../../../xraylib/) ----
            "xrayStart" -> {
                val config = call.argument<String>("config") ?: ""
                try {
                    xraylib.Xraylib.start(config, socketBinder)
                    result.success(null)
                } catch (e: Throwable) {
                    result.success(e.message ?: e.toString())
                }
            }
            "xrayStop" -> {
                runCatching { xraylib.Xraylib.stop() }
                // How many of the core's sockets actually went out over the
                // cellular network during this session -- the honest answer
                // to "was this check done over mobile data".
                result.success(mapOf("bound" to bindOk.getAndSet(0), "unbound" to bindFail.getAndSet(0), "error" to lastBindError))
            }
            "xrayVersion" -> result.success(runCatching { xraylib.Xraylib.version() }.getOrDefault(""))
            else -> result.notImplemented()
        }
    }

    // ---- connectivity ----

    private fun cm(): ConnectivityManager =
        context.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager

    /** True when ANY network with internet access rides on cellular -- not
     *  just the default one, because while a third-party VPN is active the
     *  default network is the VPN, and on Android < 12 its capabilities do
     *  not carry the underlying transport. */
    private fun hasMobileData(): Boolean = findCellular() != null

    private fun hasActiveVpn(): Boolean {
        val cm = cm()
        val net = cm.activeNetwork ?: return false
        val caps = cm.getNetworkCapabilities(net) ?: return false
        return caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
    }

    private fun networkState(): Map<String, Any?> =
        mapOf("mobile" to hasMobileData(), "vpn" to hasActiveVpn(), "mobileSetting" to mobileDataSetting())

    override fun onListen(arguments: Any?, events: EventChannel.EventSink?) {
        if (events == null) return
        stopWatching()
        val cb = object : ConnectivityManager.NetworkCallback() {
            private fun push() { mainHandler.post { events.success(networkState()) } }
            override fun onAvailable(network: Network) = push()
            override fun onLost(network: Network) = push()
            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) = push()
        }
        networkCallback = cb
        try {
            // Any network, not just the default: a cellular network coming
            // up behind a VPN is exactly the event the home screen wants.
            cm().registerNetworkCallback(
                android.net.NetworkRequest.Builder()
                    .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                    .build(),
                cb,
            )
        } catch (_: Exception) {
            networkCallback = null
        }
        events.success(networkState())
    }

    override fun onCancel(arguments: Any?) = stopWatching()

    private fun stopWatching() {
        networkCallback?.let { try { cm().unregisterNetworkCallback(it) } catch (_: Exception) {} }
        networkCallback = null
    }

    // ---- cellular: keep a mobile-data Network around and pin sockets to it ----

    @Volatile private var cellular: Network? = null
    private var cellularCb: ConnectivityManager.NetworkCallback? = null
    @Volatile private var cellularRequestError: String? = null

    /** requestNetwork() both tells us WHICH Network is the cellular one and
     *  asks the system to keep mobile data up even while Wi-Fi is the
     *  default -- without it the phone may drop the cellular link entirely. */
    private fun ensureCellularRequest() {
        if (cellularCb != null) return
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) { cellular = network }
            override fun onLost(network: Network) { if (cellular == network) cellular = null }
        }
        val req = NetworkRequest.Builder()
            .addTransportType(NetworkCapabilities.TRANSPORT_CELLULAR)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        try {
            cm().requestNetwork(req, cb)
            cellularCb = cb
            cellularRequestError = null
        } catch (e: Exception) {
            // Without CHANGE_NETWORK_STATE this is a SecurityException -- and
            // then a Wi-Fi phone never brings mobile data up for us.
            cellularRequestError = e.javaClass.simpleName + ": " + (e.message ?: "")
        }
    }

    /** The user's "mobile data" switch, regardless of whether the cellular
     *  network is currently up (Wi-Fi phones keep it down until asked). */
    private fun mobileDataSetting(): Boolean? = try {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            (context.getSystemService(Context.TELEPHONY_SERVICE) as TelephonyManager).isDataEnabled
        } else null
    } catch (_: Exception) { null }

    /** Any live non-VPN network with internet (Wi-Fi, ethernet…) -- for the
     *  bypass probe when cellular is down: a VPN that forbids bypass refuses
     *  a socket bound to ANY other network, not just the cellular one. */
    private fun findAnyNonVpn(): Network? {
        val cm = cm()
        val nets: Array<Network> = try { cm.allNetworks } catch (_: Exception) { emptyArray() }
        for (n in nets) {
            val caps = cm.getNetworkCapabilities(n) ?: continue
            if (caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
                !caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
            ) return n
        }
        return null
    }

    private fun releaseCellularRequest() {
        cellularCb?.let { runCatching { cm().unregisterNetworkCallback(it) } }
        cellularCb = null
        cellular = null
    }

    private fun waitForCellular(maxWaitMs: Long): Network? {
        val deadline = System.currentTimeMillis() + maxWaitMs
        while (true) {
            findCellular()?.let { return it }
            if (System.currentTimeMillis() >= deadline) return null
            try { Thread.sleep(250) } catch (_: InterruptedException) { return null }
        }
    }

    /** Is a VPN holding the phone in "block connections without VPN"
     *  (always-on lockdown / kill switch) mode? Two signals: the system
     *  setting where readable, and a behavioural probe -- a socket bound to
     *  the cellular network that cannot even start a TCP handshake while a
     *  VPN is up is exactly what lockdown looks like to an app. */
    private fun vpnLockdown(): Map<String, Any?> {
        val vpn = hasActiveVpn()
        if (!vpn) return mapOf("lockdown" to false, "vpn" to false, "detail" to "no vpn")
        val settingOn = try {
            Settings.Secure.getInt(context.contentResolver, "always_on_vpn_lockdown", 0) == 1
        } catch (_: Exception) { false }
        if (settingOn) return mapOf("lockdown" to true, "vpn" to true, "detail" to "always_on_vpn_lockdown=1")
        // Cellular preferred (that is what the checks use); if it is down the
        // probe goes over whatever non-VPN network exists -- the EPERM a
        // no-bypass VPN answers with is the same on every network.
        val cellNet = waitForCellular(3000)
        val net = cellNet ?: findAnyNonVpn()
            ?: return mapOf("lockdown" to false, "vpn" to true,
                            "detail" to "no non-vpn network (cellularRequest=${cellularRequestError ?: "ok"})")
        val via = if (cellNet != null) "cellular" else "other"
        // What the cellular network actually offers (v4/v6 addresses, routes,
        // interface) -- an IPv6-only APN without 464xlat makes IPv4 targets
        // "unreachable" and must not be mistaken for a kill switch.
        val lp = runCatching { cm().getLinkProperties(net) }.getOrNull()
        val addrs = lp?.linkAddresses?.joinToString(",") { it.address.hostAddress ?: "?" } ?: "?"
        val iface = lp?.interfaceName ?: "?"
        // IPv4 only -- that is what every check uses.
        val v4 = try {
            val sock = net.socketFactory.createSocket()
            try { sock.connect(java.net.InetSocketAddress("8.8.8.8", 53), 4000); "ok" } finally { runCatching { sock.close() } }
        } catch (e: Exception) { (e.message ?: e.javaClass.simpleName).take(80) }
        val detail = "via=$via iface=$iface addrs=$addrs v4=$v4 cellularRequest=${cellularRequestError ?: "ok"}"
        // Kill switch = the system refuses the connection outright (EPERM).
        // "unreachable" means the cellular network has no IPv4 route right
        // now -- a different problem, reported but not called a kill switch.
        val lockdown = v4 != "ok" && (v4.contains("EPERM", true) || v4.contains("Permission denied", true))
        return mapOf("lockdown" to lockdown, "vpn" to true, "detail" to detail)
    }

    /** One HTTPS POST over the cellular network specifically
     *  (Network.openConnection), whatever the default route is -- Wi-Fi or
     *  a VPN. Used for the network report: the server records the public IP
     *  the mobile operator gave us. Blocking; call off the main thread. */
    private fun cellularPost(url: String, body: String, token: String?, waitMs: Long): Map<String, Any?> {
        ensureCellularRequest()
        val net = waitForCellular(waitMs) ?: return mapOf("ok" to false, "status" to 0, "error" to "no cellular network")
        var conn: java.net.HttpURLConnection? = null
        return try {
            conn = net.openConnection(java.net.URL(url)) as java.net.HttpURLConnection
            conn.requestMethod = "POST"
            conn.connectTimeout = 10000
            conn.readTimeout = 10000
            conn.doOutput = true
            conn.setRequestProperty("Content-Type", "application/json")
            if (token != null) conn.setRequestProperty("Authorization", "Bearer $token")
            conn.outputStream.use { it.write(body.toByteArray(Charsets.UTF_8)) }
            val code = conn.responseCode
            val text = runCatching {
                (if (code >= 400) conn.errorStream else conn.inputStream)?.bufferedReader()?.use { it.readText() } ?: ""
            }.getOrDefault("")
            mapOf("ok" to (code in 200..299), "status" to code, "body" to text.take(2000), "error" to null)
        } catch (e: Exception) {
            mapOf("ok" to false, "status" to 0, "error" to (e.javaClass.simpleName + ": " + (e.message ?: "")).take(300))
        } finally {
            conn?.disconnect()
        }
    }

    /** «Белые списки» detection over the cellular network specifically:
     *  under the Russian mobile whitelist mode only allowed IPs are routed at
     *  all (an L3 block at the towers -- even ICMP to google.com dies), so
     *  `allowed` hosts (ya.ru, vk.ru, mail.ru) must answer and neutral
     *  `control` hosts must not. Each host: DNS via the cellular network,
     *  then ICMP echo and a TCP connect to :443 in parallel; reachable = any
     *  of the two answered. Blocking; call off the main thread. */
    private fun whitelistProbe(allowed: List<String>, control: List<String>, timeoutMs: Int): Map<String, Any?> {
        val net = waitForCellular(3000) ?: return mapOf("verdict" to "no_cellular", "allowed" to emptyList<Any>(), "control" to emptyList<Any>())
        val pool = java.util.concurrent.Executors.newFixedThreadPool(12)
        try {
            fun probeHost(host: String): java.util.concurrent.Future<Map<String, Any?>> = pool.submit(java.util.concurrent.Callable {
                val addr = try {
                    net.getAllByName(host).firstOrNull { it is java.net.Inet4Address }
                } catch (_: Exception) { null }
                if (addr == null) {
                    mapOf("host" to host, "ip" to null, "icmp_ms" to null, "tcp_ms" to null, "ok" to false, "error" to "dns")
                } else {
                    val icmp = pool.submit(java.util.concurrent.Callable { icmpPing(net, addr, timeoutMs) })
                    val tcp = pool.submit(java.util.concurrent.Callable { tcpConnect(net, addr, 443, timeoutMs) })
                    val icmpMs = runCatching { icmp.get(timeoutMs + 1000L, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrNull()
                    val tcpMs = runCatching { tcp.get(timeoutMs + 1000L, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrNull()
                    mapOf("host" to host, "ip" to addr.hostAddress, "icmp_ms" to icmpMs, "tcp_ms" to tcpMs,
                          "ok" to (icmpMs != null || tcpMs != null), "error" to null)
                }
            })
            val a = allowed.map { probeHost(it) }
            val c = control.map { probeHost(it) }
            val wait = (timeoutMs * 2 + 3000).toLong()
            val allowedRes = a.map { runCatching { it.get(wait, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrElse { e -> mapOf("ok" to false, "error" to e.javaClass.simpleName) } }
            val controlRes = c.map { runCatching { it.get(wait, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrElse { e -> mapOf("ok" to false, "error" to e.javaClass.simpleName) } }
            val allowedOk = allowedRes.count { it["ok"] == true }
            val controlOk = controlRes.count { it["ok"] == true }
            val verdict = when {
                allowed.isEmpty() || control.isEmpty() -> "unknown"
                allowedOk == 0 -> "no_internet"
                controlOk == 0 -> "active"
                controlOk == control.size -> "inactive"
                else -> "partial"
            }
            return mapOf("verdict" to verdict, "allowed" to allowedRes, "control" to controlRes)
        } finally {
            pool.shutdownNow()
        }
    }

    /** ICMP echo + TCP connect to :443 of the checked resource itself over
     *  the cellular network, the same measurement as [whitelistProbe] does
     *  per host. Blocking; call off the main thread. */
    private fun hostProbe(host: String, timeoutMs: Int): Map<String, Any?> {
        val net = waitForCellular(3000) ?: return mapOf("host" to host, "ip" to null, "icmp_ms" to null, "tcp_ms" to null, "error" to "no_cellular")
        val addr = try {
            net.getAllByName(host).firstOrNull { it is java.net.Inet4Address }
        } catch (_: Exception) { null }
            ?: return mapOf("host" to host, "ip" to null, "icmp_ms" to null, "tcp_ms" to null, "error" to "dns")
        val pool = java.util.concurrent.Executors.newFixedThreadPool(2)
        try {
            val icmp = pool.submit(java.util.concurrent.Callable { icmpPing(net, addr, timeoutMs) })
            val tcp = pool.submit(java.util.concurrent.Callable { tcpConnect(net, addr, 443, timeoutMs) })
            val icmpMs = runCatching { icmp.get(timeoutMs + 1000L, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrNull()
            val tcpMs = runCatching { tcp.get(timeoutMs + 1000L, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrNull()
            return mapOf("host" to host, "ip" to addr.hostAddress, "icmp_ms" to icmpMs, "tcp_ms" to tcpMs, "error" to null)
        } finally {
            pool.shutdownNow()
        }
    }

    /** Walks every address of a /24 over the cellular link: ICMP echo plus a
     *  TCP handshake on 443 and 80. Answers the question a single-address
     *  check cannot -- whether the operator drops the whole subnet or just
     *  some hosts in it -- so it has to visit all 256, not a sample.
     *
     *  Runs in its own pool rather than one hostProbe per address: 256
     *  sequential probes would take minutes on a mobile link, and each
     *  hostProbe also re-resolves DNS it does not need here. */
    private fun subnetProbe(cidr: String, timeoutMs: Int, concurrency: Int): Map<String, Any?> {
        val base = cidr.substringBefore('/')
        val octets = base.split('.')
        if (octets.size != 4) return mapOf("cidr" to cidr, "error" to "bad_cidr")
        val prefix = octets.take(3).joinToString(".")
        val net = waitForCellular(3000)
            ?: return mapOf("cidr" to cidr, "error" to "no_cellular")

        val pool = java.util.concurrent.Executors.newFixedThreadPool(concurrency.coerceIn(1, 64))
        try {
            val tasks = (0..255).map { last ->
                java.util.concurrent.Callable {
                    val addr = java.net.InetAddress.getByName("$prefix.$last")
                    val icmp = icmpPing(net, addr, timeoutMs)
                    // 443 first: an https host is the common case, and 80 is
                    // only worth the extra handshake when 443 stays silent.
                    val tcp443 = tcpConnect(net, addr, 443, timeoutMs)
                    val tcp80 = if (tcp443 == null) tcpConnect(net, addr, 80, timeoutMs) else null
                    mapOf(
                        "ip" to "$prefix.$last",
                        "icmp_ms" to icmp,
                        "tcp443_ms" to tcp443,
                        "tcp80_ms" to tcp80,
                    )
                }
            }
            val started = System.nanoTime()
            // One bounded wait for the whole sweep: a stuck address must not
            // hold the job open past the lease the server granted.
            val futures = pool.invokeAll(tasks, (timeoutMs.toLong() * 12) + 20_000, java.util.concurrent.TimeUnit.MILLISECONDS)
            val hosts = ArrayList<Map<String, Any?>>()
            var probed = 0
            for (f in futures) {
                val r = runCatching { f.get() }.getOrNull() ?: continue
                probed++
                if (r["icmp_ms"] != null || r["tcp443_ms"] != null || r["tcp80_ms"] != null) hosts.add(r)
            }
            return mapOf(
                "cidr" to cidr,
                "probed" to probed,
                "total" to 256,
                "alive" to hosts.size,
                "alive_icmp" to hosts.count { it["icmp_ms"] != null },
                "alive_tcp" to hosts.count { it["tcp443_ms"] != null || it["tcp80_ms"] != null },
                // The live ones only: 256 rows per job would bloat every
                // result row in the database for no added meaning.
                "hosts" to hosts.take(40),
                "elapsed_ms" to (System.nanoTime() - started) / 1_000_000L,
                "error" to null,
            )
        } catch (e: Exception) {
            return mapOf("cidr" to cidr, "error" to (e.message ?: e.toString()))
        } finally {
            pool.shutdownNow()
        }
    }

    /** ICMP echo through an unprivileged ping socket bound to [net]; the
     *  kernel fills in the identifier and checksum. Round-trip ms or null. */
    private fun icmpPing(net: Network, addr: java.net.InetAddress, timeoutMs: Int): Long? {
        val fd = try { Os.socket(OsConstants.AF_INET, OsConstants.SOCK_DGRAM, OsConstants.IPPROTO_ICMP) } catch (_: Exception) { return null }
        try {
            net.bindSocket(fd)
            val req = ByteArray(16).also { it[0] = 8; it[7] = 1 } // echo request, seq 1
            val start = System.nanoTime()
            Os.sendto(fd, req, 0, req.size, 0, addr, 0)
            val deadline = start + timeoutMs * 1_000_000L
            val buf = ByteArray(128)
            while (true) {
                val left = ((deadline - System.nanoTime()) / 1_000_000L).toInt()
                if (left <= 0) return null
                val pfd = android.system.StructPollfd().apply { this.fd = fd; events = OsConstants.POLLIN.toShort() }
                if (Os.poll(arrayOf(pfd), left) <= 0) return null
                val len = Os.recvfrom(fd, buf, 0, buf.size, 0, null)
                if (len >= 8 && buf[0].toInt() == 0) return (System.nanoTime() - start) / 1_000_000L // echo reply
            }
        } catch (_: Exception) {
            return null
        } finally {
            runCatching { Os.close(fd) }
        }
    }

    private fun tcpConnect(net: Network, addr: java.net.InetAddress, port: Int, timeoutMs: Int): Long? = try {
        val start = System.nanoTime()
        net.socketFactory.createSocket().use { it.connect(java.net.InetSocketAddress(addr, port), timeoutMs) }
        (System.nanoTime() - start) / 1_000_000L
    } catch (_: Exception) {
        null
    }

    private fun operatorName(): String? = try {
        val tm = context.getSystemService(Context.TELEPHONY_SERVICE) as TelephonyManager
        tm.networkOperatorName?.takeIf { it.isNotBlank() } ?: tm.simOperatorName?.takeIf { it.isNotBlank() }
    } catch (_: Exception) {
        null
    }

    private val bindOk = AtomicInteger(0)
    private val bindFail = AtomicInteger(0)
    @Volatile private var lastBindError: String? = null

    /** The cellular Network object: the one requestNetwork() handed us, or
     *  any live cellular+internet network that is not itself a VPN. */
    private fun findCellular(): Network? {
        cellular?.let { n ->
            val caps = cm().getNetworkCapabilities(n)
            if (caps != null && caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) return n
        }
        val cm = cm()
        val nets: Array<Network> = try { cm.allNetworks } catch (_: Exception) { emptyArray() }
        for (n in nets) {
            val caps = cm.getNetworkCapabilities(n) ?: continue
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) &&
                caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
                !caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
            ) return n
        }
        return null
    }

    /** Called by Xray (Go, from its own threads) for every socket it is
     *  about to connect: Network.bindSocket routes that socket through the
     *  cellular network regardless of the process default / any VPN. Go
     *  talks to the kernel directly, so bindProcessToNetwork alone would not
     *  cover these sockets -- this callback is why Xray is embedded. */
    private val socketBinder = object : xraylib.SocketBinder {
        override fun bind(fd: Long): Boolean {
            val net = findCellular()
            val pfd = try { ParcelFileDescriptor.fromFd(fd.toInt()) } catch (_: Exception) { null }
            if (net == null || pfd == null) {
                lastBindError = if (net == null) "no cellular network" else "bad fd"
                bindFail.incrementAndGet()
                pfd?.let { killSocket(it) }
                return false
            }
            return try {
                net.bindSocket(pfd.fileDescriptor)
                bindOk.incrementAndGet()
                pfd.close()
                true
            } catch (e: Exception) {
                lastBindError = e.javaClass.simpleName + ": " + (e.message ?: "")
                bindFail.incrementAndGet()
                // Xray-core logs a controller error and dials anyway (see
                // transport/internet/system_dialer.go) -- which would send
                // the check through the user's VPN. Shut the socket so the
                // dial fails instead: no binding, no connection.
                killSocket(pfd)
                false
            }
        }
    }

    private fun killSocket(pfd: ParcelFileDescriptor) {
        try { Os.shutdown(pfd.fileDescriptor, OsConstants.SHUT_RDWR) } catch (_: Exception) {}
        try { pfd.close() } catch (_: Exception) {}
    }

    /** The system page where the user can re-enable notifications for this
     *  app after a "don't ask again" denial (the runtime prompt won't show
     *  a second time). */
    private fun openNotificationSettings(): Boolean {
        return try {
            val intent = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                    .putExtra(Settings.EXTRA_APP_PACKAGE, context.packageName)
            } else {
                Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:" + context.packageName))
            }
            intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            context.startActivity(intent)
            true
        } catch (_: Exception) {
            false
        }
    }

    // ---- hardware id ----

    /** sha256 of the Widevine device-unique id (stable across app reinstalls
     *  AND factory resets on virtually every phone), falling back to
     *  ANDROID_ID (stable across reinstalls of this app). Sent to the server
     *  at sign-in and with every heartbeat; the admin panel bans by it. */
    private fun hwid(): String {
        val widevine = try {
            val uuid = UUID(-0x121074568629b532L, -0x5c37d8232ae2de13L)
            val drm = MediaDrm(uuid)
            try {
                drm.getPropertyByteArray(MediaDrm.PROPERTY_DEVICE_UNIQUE_ID)
            } finally {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) drm.close() else @Suppress("DEPRECATION") drm.release()
            }
        } catch (_: Throwable) {
            null
        }
        val androidId = try {
            Settings.Secure.getString(context.contentResolver, Settings.Secure.ANDROID_ID) ?: ""
        } catch (_: Exception) {
            ""
        }
        val material = if (widevine != null && widevine.isNotEmpty()) "wv:".toByteArray() + widevine
                       else "aid:$androidId".toByteArray()
        val digest = MessageDigest.getInstance("SHA-256").digest(material)
        return digest.joinToString("") { "%02x".format(it) }
    }

    // ---- Android Key Attestation ----

    /** Generates a throw-away EC key inside the phone's secure hardware with
     *  the server's challenge baked in and returns its certificate chain
     *  (leaf first, base64 DER). The leaf carries the attestation extension
     *  the server verifies (services/keyAttestation.js): package name, our
     *  signing-cert digest, verified-boot state. Throws where the device
     *  cannot attest at all (no TEE support, no Google root -- Huawei w/o GMS). */
    private fun attest(challenge: ByteArray): List<String> {
        val alias = "svyazest_attest"
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        runCatching { ks.deleteEntry(alias) }
        val spec = KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setAttestationChallenge(challenge)
            .build()
        val kpg = KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore")
        kpg.initialize(spec)
        kpg.generateKeyPair()
        val chain = ks.getCertificateChain(alias) ?: throw IllegalStateException("no certificate chain")
        val out = chain.map { Base64.encodeToString(it.encoded, Base64.NO_WRAP) }
        runCatching { ks.deleteEntry(alias) }
        return out
    }

    // ---- root detection ----

    private fun isRooted(): Boolean {
        val paths = listOf(
            "/system/bin/su", "/system/xbin/su", "/sbin/su", "/su/bin/su", "/system/sbin/su",
            "/vendor/bin/su", "/system/xbin/daemonsu", "/system/app/Superuser.apk",
            "/system/app/SuperSU", "/system/app/SuperSU.apk",
            "/data/adb/magisk", "/data/adb/ksu", "/data/adb/modules", "/data/adb/ap",
        )
        if (paths.any { runCatching { File(it).exists() }.getOrDefault(false) }) return true
        System.getenv("PATH")?.split(":")?.forEach { dir ->
            if (runCatching { File(dir, "su").exists() }.getOrDefault(false)) return true
        }
        if (Build.TAGS?.contains("test-keys") == true) return true
        val managers = listOf(
            "com.topjohnwu.magisk", "io.github.huskydg.magisk", "me.weishu.kernelsu",
            "me.bmax.apatch", "eu.chainfire.supersu", "com.koushikdutta.superuser",
            "com.noshufou.android.su", "com.thirdparty.superuser",
        )
        for (pkg in managers) {
            if (runCatching { context.packageManager.getPackageInfo(pkg, 0) }.isSuccess) return true
        }
        return false
    }
}
