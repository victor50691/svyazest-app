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
        // Yandex DNS: a resolver of its own for the DNS check, and one of
        // the addresses a whitelist keeps reachable.
        const val FALLBACK_DNS = "77.88.8.8"
        const val DEVICE_KEY_ALIAS = "svyazest_device_key"
        val DEVICE_KEY_LOCK = Any()
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

    /** SHA-256 hex of the signing certificates of a package / an APK. */
    @Suppress("DEPRECATION")
    private fun signerDigests(info: android.content.pm.PackageInfo?): List<String> {
        val sigs = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            info?.signingInfo?.let { if (it.hasMultipleSigners()) it.apkContentsSigners else it.signingCertificateHistory }
        } else info?.signatures
        return sigs?.map { s -> MessageDigest.getInstance("SHA-256").digest(s.toByteArray()).joinToString("") { "%02x".format(it) } }
            ?: emptyList()
    }

    @Suppress("DEPRECATION")
    private fun apkCheck(path: String): Map<String, Any?> {
        val f = File(path)
        if (!f.exists()) return mapOf("ok" to false, "error" to "no file")
        val pm = context.packageManager
        val flags = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) android.content.pm.PackageManager.GET_SIGNING_CERTIFICATES
                    else android.content.pm.PackageManager.GET_SIGNATURES
        val apk = pm.getPackageArchiveInfo(path, flags) ?: return mapOf("ok" to false, "error" to "not an apk")
        val mine = pm.getPackageInfo(context.packageName, flags)
        val apkSigners = signerDigests(apk)
        val mySigners = signerDigests(mine)
        val sha = MessageDigest.getInstance("SHA-256").let { md ->
            f.inputStream().use { s -> val b = ByteArray(64 * 1024); while (true) { val n = s.read(b); if (n < 0) break; md.update(b, 0, n) } }
            md.digest().joinToString("") { "%02x".format(it) }
        }
        val code = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) apk.longVersionCode else apk.versionCode.toLong()
        val myCode = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) mine.longVersionCode else mine.versionCode.toLong()
        val samePackage = apk.packageName == context.packageName
        // Android < 9 may give no signatures for an archive (v2-only APK): the
        // SHA-256 from the server checks it then, and the system installer
        // refuses an update signed by anyone else in any case.
        val sameSigner = (apkSigners.isNotEmpty() && apkSigners.any { it in mySigners }) ||
            (apkSigners.isEmpty() && Build.VERSION.SDK_INT < Build.VERSION_CODES.P)
        return mapOf(
            "ok" to (samePackage && sameSigner),
            "package_ok" to samePackage, "signer_ok" to sameSigner,
            "version_name" to apk.versionName, "version_code" to code, "installed_code" to myCode,
            "sha256" to sha, "size" to f.length(),
        )
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
            "signRequest" -> Thread {
                val sig = signRequest(call.argument<String>("payload") ?: "")
                mainHandler.post { result.success(sig) }
            }.start()
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
                        NativeLog.add("мобильная", if (ok) "процесс привязан к мобильной сети $net" else "привязать процесс к мобильной сети не удалось: $error")
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
                val family = call.argument<Int>("family") ?: 4
                Thread {
                    val reply = hostProbe(host, timeoutMs, family)
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            "cellularIpv6" -> result.success(cellularIpv6())
            "subnetProbe" -> {
                val cidr = call.argument<String>("cidr") ?: ""
                val timeoutMs = call.argument<Int>("timeoutMs") ?: 1200
                val concurrency = call.argument<Int>("concurrency") ?: 32
                Thread {
                    val reply = subnetProbe(cidr, timeoutMs, concurrency)
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            // ---- diagnostics log, native half (NativeLog.kt) ----
            "diagNativeEnable" -> { NativeLog.setEnabled(context, call.argument<Boolean>("on") == true); result.success(null) }
            "diagNativeDrain" -> result.success(NativeLog.drain())
            "diagEnv" -> Thread {
                val lines = runCatching { NativeLog.env(context) }.getOrElse { listOf("снимок: ${it.javaClass.simpleName}") }
                mainHandler.post { result.success(lines) }
            }.start()
            "diagPastExits" -> result.success(runCatching { NativeLog.pastExits(context) }.getOrElse { emptyList() })
            // ---- link to our server, separate from the checks (ControlChannel.kt) ----
            "controlRequest" -> {
                val method = call.argument<String>("method") ?: "GET"
                val url = call.argument<String>("url") ?: ""
                @Suppress("UNCHECKED_CAST")
                val headers = (call.argument<Map<String, String>>("headers") ?: emptyMap())
                val body = call.argument<ByteArray>("body")
                val readMs = call.argument<Int>("readTimeoutMs") ?: 60000
                Thread {
                    val reply = try {
                        ControlChannel.request(context, method, url, headers, body, readMs)
                    } catch (e: Throwable) {
                        mapOf("status" to 0, "error" to (e.javaClass.simpleName + ": " + (e.message ?: "")).take(200))
                    }
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            // ── In-app update (1.19.0) ─────────────────────────────────
            // Download the APK over the same server routes as everything else.
            "controlDownload" -> {
                val url = call.argument<String>("url") ?: ""
                @Suppress("UNCHECKED_CAST")
                val headers = (call.argument<Map<String, String>>("headers") ?: emptyMap())
                val path = call.argument<String>("path") ?: ""
                Thread {
                    val reply = try {
                        ControlChannel.request(context, "GET", url, headers, null, 60000, File(path))
                    } catch (e: Throwable) {
                        mapOf("status" to 0, "error" to (e.javaClass.simpleName + ": " + (e.message ?: "")).take(200))
                    }
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            // What the downloaded file is: our package, which version, signed
            // with the same certificate as the installed app, its SHA-256.
            "apkCheck" -> {
                val path = call.argument<String>("path") ?: ""
                Thread {
                    val reply = try { apkCheck(path) } catch (e: Throwable) {
                        mapOf("ok" to false, "error" to (e.javaClass.simpleName + ": " + (e.message ?: "")).take(200))
                    }
                    mainHandler.post { result.success(reply) }
                }.start()
            }
            "canInstallApks" -> result.success(
                Build.VERSION.SDK_INT < Build.VERSION_CODES.O || context.packageManager.canRequestPackageInstalls())
            "openInstallSettings" -> {
                try {
                    val i = Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:" + context.packageName))
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                    context.startActivity(i)
                    result.success(true)
                } catch (e: Throwable) {
                    result.success(false)
                }
            }
            "installApk" -> {
                try {
                    val f = File(call.argument<String>("path") ?: "")
                    val uri = androidx.core.content.FileProvider.getUriForFile(context, context.packageName + ".updates", f)
                    val i = Intent(Intent.ACTION_VIEW)
                        .setDataAndType(uri, "application/vnd.android.package-archive")
                        .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
                    context.startActivity(i)
                    result.success(true)
                } catch (e: Throwable) {
                    result.success(false)
                }
            }
            "controlInfo" -> result.success(ControlChannel.info(context))
            "controlScanProxy" -> Thread {
                val r = runCatching { ControlChannel.scanNow(context) }.getOrElse { mapOf("found" to null, "report" to listOf("ошибка: ${it.javaClass.simpleName}"), "ports" to "") }
                mainHandler.post { result.success(r) }
            }.start()
            "controlSetProxyPort" -> { ControlChannel.setManualPort(context, call.argument<Int>("port") ?: 0); result.success(null) }
            "controlSetProxy" -> {
                ControlChannel.setProxy(context, call.argument<Int>("port") ?: 0,
                    call.argument<String>("user") ?: "", call.argument<String>("pass") ?: "")
                result.success(null)
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
            "cellularGet" -> {
                val url = call.argument<String>("url") ?: ""
                val headers = call.argument<Map<String, String>>("headers") ?: emptyMap()
                val waitMs = (call.argument<Int>("waitMs") ?: 0).toLong()
                Thread {
                    val reply = cellularGet(url, headers, waitMs)
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
                if (call.argument<String>("core") == "next") {
                    // Xray-core 26.10 as its own process (XrayNext.kt).
                    Thread {
                        val err = runCatching { XrayNext.start(context, config, findCellular()) }.getOrElse { "xray 26.10: ${it.javaClass.simpleName}" }
                        mainHandler.post { result.success(err) }
                    }.start()
                    return
                }
                try {
                    xraylib.Xraylib.start(config, socketBinder)
                    NativeLog.add("xray", "ядро запущено")
                    result.success(null)
                } catch (e: Throwable) {
                    NativeLog.add("xray", "ядро не запустилось (${e.javaClass.simpleName})")
                    result.success(e.message ?: e.toString())
                }
            }
            "xrayStop" -> {
                if (XrayNext.isRunning() || call.argument<String>("core") == "next") {
                    Thread {
                        val r = runCatching { XrayNext.stop() }.getOrElse { mapOf("bound" to 0, "unbound" to 0, "error" to it.javaClass.simpleName) }
                        mainHandler.post { result.success(r) }
                    }.start()
                    return
                }
                runCatching { xraylib.Xraylib.stop() }
                NativeLog.add("xray", "ядро остановлено: сокетов через мобильную ${bindOk.get()}, не привязано ${bindFail.get()}${lastBindError?.let { ", ошибка: $it" } ?: ""}")
                // How many of the core's sockets actually went out over the
                // cellular network during this session -- the honest answer
                // to "was this check done over mobile data".
                result.success(mapOf("bound" to bindOk.getAndSet(0), "unbound" to bindFail.getAndSet(0), "error" to lastBindError))
            }
            "xrayVersion" -> Thread {
                val main = runCatching { xraylib.Xraylib.version() }.getOrDefault("")
                val next = runCatching { XrayNext.version(context) }.getOrDefault("")
                mainHandler.post { result.success(listOf(main, next).filter { it.isNotEmpty() }.joinToString(" · ")) }
            }.start()
            // «Разрешить сбор логов?» (DiagNotifier.kt, lib/services/diag_log.dart).
            "diagShowRequest" -> { DiagNotifier.show(context, call.argument<Int>("id") ?: -1, call.argument<String>("reason") ?: ""); result.success(null) }
            "diagCancel" -> { DiagNotifier.cancel(context); result.success(null) }
            "diagPeekDecision" -> result.success(DiagNotifier.peekDecision(context))
            "diagClearDecision" -> { DiagNotifier.clearDecision(context, call.argument<Int>("id") ?: -1); result.success(null) }
            // «Подтвердить местоположение?» notification (GpsPlugin.kt).
            "gpsShowRequest" -> { GpsNotifier.show(context, call.argument<Int>("id") ?: -1, call.argument<String>("reason") ?: ""); result.success(null) }
            "gpsCancel" -> { GpsNotifier.cancel(context); result.success(null) }
            "gpsPeekDecline" -> result.success(GpsNotifier.peekDecline(context))
            "gpsClearDecline" -> { GpsNotifier.clearDecline(context, call.argument<Int>("id") ?: -1); result.success(null) }
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
            override fun onAvailable(network: Network) { cellular = network; NativeLog.add("мобильная", "мобильная сеть поднята для приложения: $network") }
            override fun onLost(network: Network) { if (cellular == network) cellular = null; NativeLog.add("мобильная", "мобильная сеть пропала: $network") }
            override fun onUnavailable() { NativeLog.add("мобильная", "система не дала мобильную сеть (onUnavailable)") }
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
            NativeLog.add("мобильная", "запрос мобильной сети отклонён: $cellularRequestError")
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

    /** One HTTPS GET over the cellular network specifically, like
     *  [cellularPost]. Used for the network report (1.17.1): Yandex's
     *  ipv4-/ipv6-internet echo gives the operator's IPv4 and IPv6 separately
     *  -- a request to our own server shows only one of them (IPv6 when the
     *  operator gives one). Blocking; call off the main thread. */
    private fun cellularGet(url: String, headers: Map<String, String>, waitMs: Long): Map<String, Any?> {
        ensureCellularRequest()
        val net = waitForCellular(waitMs) ?: return mapOf("ok" to false, "status" to 0, "error" to "no cellular network")
        var conn: java.net.HttpURLConnection? = null
        return try {
            conn = net.openConnection(java.net.URL(url)) as java.net.HttpURLConnection
            conn.requestMethod = "GET"
            conn.connectTimeout = 8000
            conn.readTimeout = 8000
            headers.forEach { (k, v) -> conn.setRequestProperty(k, v) }
            val code = conn.responseCode
            val text = runCatching {
                (if (code >= 400) conn.errorStream else conn.inputStream)?.bufferedReader()?.use { it.readText() } ?: ""
            }.getOrDefault("")
            mapOf("ok" to (code in 200..299), "status" to code, "body" to text.take(500), "error" to null)
        } catch (e: Exception) {
            mapOf("ok" to false, "status" to 0, "error" to (e.javaClass.simpleName + ": " + (e.message ?: "")).take(200))
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
     *  of the two answered. Blocking; call off the main thread.
     *
     *  DNS is measured on its own: when the phone's resolver is dead (the
     *  operator's, or a «Частный DNS» such as dns.google, which a whitelist
     *  cuts off) every check would fail for a reason that has nothing to do
     *  with the checked resource. A host the system cannot resolve is asked
     *  of [FALLBACK_DNS] directly, and a dead system resolver makes the
     *  verdict `no_dns` -- no jobs for this phone until it answers again. */
    private fun whitelistProbe(allowed: List<String>, control: List<String>, timeoutMs: Int): Map<String, Any?> {
        val net = waitForCellular(3000) ?: return mapOf("verdict" to "no_cellular", "allowed" to emptyList<Any>(), "control" to emptyList<Any>())
        val pool = java.util.concurrent.Executors.newFixedThreadPool(12)
        try {
            fun probeHost(host: String): java.util.concurrent.Future<Map<String, Any?>> = pool.submit(java.util.concurrent.Callable {
                val sys = try {
                    net.getAllByName(host).firstOrNull { it is java.net.Inet4Address }
                } catch (_: Exception) { null }
                val addr = sys ?: dnsQueryA(net, FALLBACK_DNS, host, timeoutMs)
                val dnsVia = if (sys != null) "system" else if (addr != null) "fallback" else null
                if (addr == null) {
                    mapOf("host" to host, "ip" to null, "icmp_ms" to null, "tcp_ms" to null, "ok" to false, "dns" to null, "error" to "dns")
                } else {
                    val icmp = pool.submit(java.util.concurrent.Callable { icmpPing(net, addr, timeoutMs) })
                    val tcp = pool.submit(java.util.concurrent.Callable { tcpConnect(net, addr, 443, timeoutMs) })
                    val icmpMs = runCatching { icmp.get(timeoutMs + 1000L, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrNull()
                    val tcpMs = runCatching { tcp.get(timeoutMs + 1000L, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrNull()
                    mapOf("host" to host, "ip" to addr.hostAddress, "icmp_ms" to icmpMs, "tcp_ms" to tcpMs,
                          "ok" to (icmpMs != null || tcpMs != null), "dns" to dnsVia, "error" to null)
                }
            })
            val a = allowed.map { probeHost(it) }
            val c = control.map { probeHost(it) }
            val wait = (timeoutMs * 3 + 3000).toLong()
            val allowedRes = a.map { runCatching { it.get(wait, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrElse { e -> mapOf("ok" to false, "error" to e.javaClass.simpleName) } }
            val controlRes = c.map { runCatching { it.get(wait, java.util.concurrent.TimeUnit.MILLISECONDS) }.getOrElse { e -> mapOf("ok" to false, "error" to e.javaClass.simpleName) } }
            val allowedOk = allowedRes.count { it["ok"] == true }
            val controlOk = controlRes.count { it["ok"] == true }

            val all = allowedRes + controlRes
            val dnsSystem = all.count { it["dns"] == "system" }
            val dnsFallback = all.count { it["dns"] == "fallback" }
            // Nothing resolved at all: a dead resolver or no internet. Tell
            // them apart by reaching the fallback resolver by its address.
            val resolverReachable = if (dnsSystem == 0 && dnsFallback == 0) {
                val r = java.net.InetAddress.getByName(FALLBACK_DNS)
                tcpConnect(net, r, 53, timeoutMs) != null || icmpPing(net, r, timeoutMs) != null
            } else null
            val dnsVerdict = when {
                dnsSystem > 0 -> "ok"
                dnsFallback > 0 || resolverReachable == true -> "system_down"
                else -> "down"
            }
            val lp = runCatching { cm().getLinkProperties(net) }.getOrNull()
            val privateDns = if (Build.VERSION.SDK_INT >= 28) lp?.isPrivateDnsActive else null
            val privateDnsServer = if (Build.VERSION.SDK_INT >= 28) lp?.privateDnsServerName else null

            val verdict = when {
                allowed.isEmpty() || control.isEmpty() -> "unknown"
                dnsVerdict == "system_down" -> "no_dns"
                allowedOk == 0 -> "no_internet"
                controlOk == 0 -> "active"
                controlOk == control.size -> "inactive"
                else -> "partial"
            }
            return mapOf(
                "verdict" to verdict, "allowed" to allowedRes, "control" to controlRes,
                "dns" to mapOf(
                    "verdict" to dnsVerdict, "system" to dnsSystem, "fallback" to dnsFallback, "total" to all.size,
                    "resolver" to FALLBACK_DNS, "resolver_reachable" to resolverReachable,
                    "private_dns" to privateDns, "private_dns_server" to privateDnsServer,
                ),
            )
        } finally {
            pool.shutdownNow()
        }
    }

    /** A-record lookup sent straight to [server]:53 over UDP bound to [net],
     *  bypassing the phone's own resolver. First IPv4 answer or null. */
    private fun dnsQueryA(net: Network, server: String, host: String, timeoutMs: Int): java.net.InetAddress? {
        val sock = try { java.net.DatagramSocket() } catch (_: Exception) { return null }
        try {
            net.bindSocket(sock)
            sock.soTimeout = timeoutMs
            val id = (System.nanoTime() and 0xffff).toInt()
            val q = java.io.ByteArrayOutputStream()
            q.write(id shr 8); q.write(id and 0xff)
            q.write(0x01); q.write(0x00) // recursion desired
            q.write(0); q.write(1) // one question
            repeat(6) { q.write(0) }
            for (label in host.trimEnd('.').split('.')) {
                val b = label.toByteArray(Charsets.US_ASCII)
                if (b.isEmpty() || b.size > 63) return null
                q.write(b.size); q.write(b)
            }
            q.write(0)
            q.write(0); q.write(1); q.write(0); q.write(1) // type A, class IN
            val req = q.toByteArray()
            sock.send(java.net.DatagramPacket(req, req.size, java.net.InetAddress.getByName(server), 53))
            val buf = ByteArray(1500)
            val pkt = java.net.DatagramPacket(buf, buf.size)
            sock.receive(pkt)
            val n = pkt.length
            fun u16(i: Int) = ((buf[i].toInt() and 0xff) shl 8) or (buf[i + 1].toInt() and 0xff)
            if (n < 12 || u16(0) != id || (buf[3].toInt() and 0x0f) != 0) return null
            val qd = u16(4)
            val an = u16(6)
            var p = 12
            fun skipName() {
                while (p < n) {
                    val l = buf[p].toInt() and 0xff
                    if (l == 0) { p += 1; return }
                    if (l and 0xc0 == 0xc0) { p += 2; return }
                    p += l + 1
                }
            }
            repeat(qd) { skipName(); p += 4 }
            repeat(an) {
                skipName()
                if (p + 10 > n) return null
                val type = u16(p)
                val len = u16(p + 8)
                p += 10
                if (type == 1 && len == 4 && p + 4 <= n) return java.net.InetAddress.getByAddress(buf.copyOfRange(p, p + 4))
                p += len
            }
            return null
        } catch (_: Exception) {
            return null
        } finally {
            sock.close()
        }
    }

    /** ICMP echo + TCP connect to :443 of the checked resource itself over
     *  the cellular network, the same measurement as [whitelistProbe] does
     *  per host. Blocking; call off the main thread. */
    private fun hostProbe(host: String, timeoutMs: Int, family: Int = 4): Map<String, Any?> {
        val net = waitForCellular(3000) ?: return mapOf("host" to host, "ip" to null, "icmp_ms" to null, "tcp_ms" to null, "error" to "no_cellular")
        // family 6 (1.17.2): an IPv6 address / a domain's AAAA, ICMPv6 echo.
        val bare = host.removePrefix("[").removeSuffix("]")
        val addr = try {
            net.getAllByName(bare).firstOrNull { if (family == 6) it is java.net.Inet6Address else it is java.net.Inet4Address }
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
        // IPv6 (1.17.2): ICMPv6 echo request 128 / reply 129 on an
        // unprivileged ping socket, the same way.
        val v6 = addr is java.net.Inet6Address
        val fd = try {
            if (v6) Os.socket(OsConstants.AF_INET6, OsConstants.SOCK_DGRAM, OsConstants.IPPROTO_ICMPV6)
            else Os.socket(OsConstants.AF_INET, OsConstants.SOCK_DGRAM, OsConstants.IPPROTO_ICMP)
        } catch (_: Exception) { return null }
        val echoReply = if (v6) 129 else 0
        try {
            net.bindSocket(fd)
            val req = ByteArray(16).also { it[0] = (if (v6) 128 else 8).toByte(); it[7] = 1 } // echo request, seq 1
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
                if (len >= 8 && (buf[0].toInt() and 0xff) == echoReply) return (System.nanoTime() - start) / 1_000_000L // echo reply
            }
        } catch (_: Exception) {
            return null
        } finally {
            runCatching { Os.close(fd) }
        }
    }

    /** IPv6 of the cellular network as the system sees it (1.17.2): a
     *  global address (not link-local fe80::/10, not unique-local fc00::/7)
     *  and an IPv6 default route. No traffic -- cheap enough to ask right
     *  before every IPv6 job. */
    private fun cellularIpv6(): Map<String, Any?> = try {
        val net = findCellular()
        val lp = net?.let { runCatching { cm().getLinkProperties(it) }.getOrNull() }
        if (lp == null) mapOf("global" to false, "route" to false, "error" to "no cellular network")
        else {
            val global = lp.linkAddresses.any {
                val a = it.address
                a is java.net.Inet6Address && !a.isLinkLocalAddress && !a.isLoopbackAddress && !a.isAnyLocalAddress &&
                    !a.isSiteLocalAddress && (a.address[0].toInt() and 0xfe) != 0xfc
            }
            val route = lp.routes.any { r ->
                val d = r.destination
                d.address is java.net.Inet6Address && d.prefixLength == 0
            }
            mapOf("global" to global, "route" to route, "error" to null)
        }
    } catch (e: Exception) {
        mapOf("global" to false, "route" to false, "error" to (e.javaClass.simpleName + ": " + (e.message ?: "")).take(120))
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
    // 1.16.0: the attested key is KEPT (it never leaves the secure hardware)
    // and signs every job request -- the server binds its public key on
    // /attest/verify {bind: true}. One lock for attest and sign: the UI and
    // the service may attest at the same moment.
    private fun attest(challenge: ByteArray): List<String> = synchronized(DEVICE_KEY_LOCK) {
        val alias = DEVICE_KEY_ALIAS
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        runCatching { ks.deleteEntry(alias) }
        runCatching { ks.deleteEntry("svyazest_attest") } // the old throwaway alias
        val spec = KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setAttestationChallenge(challenge)
            .build()
        val kpg = KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore")
        kpg.initialize(spec)
        kpg.generateKeyPair()
        val chain = ks.getCertificateChain(alias) ?: throw IllegalStateException("no certificate chain")
        NativeLog.add("подлинность", "ключ устройства создан заново и отправлен на проверку")
        chain.map { Base64.encodeToString(it.encoded, Base64.NO_WRAP) }
    }

    /** ECDSA-SHA256 of `payload` with the bound device key, base64; null if there is no key yet. */
    private fun signRequest(payload: String): String? = synchronized(DEVICE_KEY_LOCK) {
        try {
            val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
            val key = ks.getKey(DEVICE_KEY_ALIAS, null) as? java.security.PrivateKey ?: return null
            val s = java.security.Signature.getInstance("SHA256withECDSA")
            s.initSign(key)
            s.update(payload.toByteArray(Charsets.UTF_8))
            Base64.encodeToString(s.sign(), Base64.NO_WRAP)
        } catch (e: Exception) {
            NativeLog.add("подлинность", "подписать запрос не удалось: ${e.javaClass.simpleName}")
            null
        }
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
