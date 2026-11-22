package com.svyazest.svyazest_app

import android.app.ActivityManager
import android.app.ApplicationExitInfo
import android.app.usage.UsageStatsManager
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.BatteryManager
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import android.telephony.TelephonyManager
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * Native half of the diagnostics log (1.14.0, 2026-11-21) -- collected ONLY
 * while the partner allowed it (DiagLog.setActive → diagNativeEnable).
 *
 * Kotlin code adds lines here (ControlChannel, GPS, cellular binding, xray);
 * while enabled it also watches every network (up / lost / capabilities),
 * power and battery events. Dart drains the buffer into its own queue
 * (source «native») on every flush.
 *
 * Always on, but local only: a crash handler that keeps the last crash's
 * stack in prefs, and Android's own record of why the process ended
 * (ApplicationExitInfo) -- both are logged at the next start, and only if
 * collection is allowed then.
 *
 * Never logged: customer resources (Dart passes no targets here), public IP
 * addresses of the phone, coordinates.
 */
object NativeLog {
    private const val PREFS = "svyazest_diag_native"
    private const val MAX = 8000

    @Volatile var enabled = false
        private set
    private val buf = ArrayDeque<Pair<Long, String>>()
    private var netCb: ConnectivityManager.NetworkCallback? = null
    private var receiver: BroadcastReceiver? = null

    fun add(tag: String, msg: String) {
        if (!enabled) return
        synchronized(buf) {
            buf.addLast(System.currentTimeMillis() to "[$tag] $msg")
            while (buf.size > MAX) buf.removeFirst()
        }
    }

    fun drain(): List<Map<String, Any>> = synchronized(buf) {
        val out = buf.map { mapOf("t" to it.first, "m" to it.second) }
        buf.clear()
        out
    }

    fun setEnabled(ctx: Context, on: Boolean) {
        if (on == enabled) return
        enabled = on
        if (on) {
            add("журнал", "нативный журнал включён")
            startWatchers(ctx.applicationContext)
        } else {
            stopWatchers(ctx.applicationContext)
            synchronized(buf) { buf.clear() }
        }
    }

    // ---------------------------------------------------------------- networks

    private fun transports(c: NetworkCapabilities?): String {
        if (c == null) return "?"
        val t = mutableListOf<String>()
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) t.add("мобильная")
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) t.add("Wi-Fi")
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) t.add("VPN")
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) t.add("ethernet")
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH)) t.add("bluetooth")
        return t.joinToString("+").ifEmpty { "другая" }
    }

    fun describeCaps(c: NetworkCapabilities?): String {
        if (c == null) return "нет данных"
        val f = mutableListOf(transports(c))
        f.add(if (c.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) "интернет" else "без интернета")
        if (c.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)) f.add("проверена") else f.add("не проверена")
        if (!c.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)) f.add("лимитная")
        if (Build.VERSION.SDK_INT >= 28 && !c.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_SUSPENDED)) f.add("ПРИОСТАНОВЛЕНА")
        if (c.hasCapability(NetworkCapabilities.NET_CAPABILITY_CAPTIVE_PORTAL)) f.add("страница входа")
        f.add("↓${c.linkDownstreamBandwidthKbps}/↑${c.linkUpstreamBandwidthKbps} кбит/с")
        if (Build.VERSION.SDK_INT >= 29 && c.signalStrength != Int.MIN_VALUE) f.add("сигнал ${c.signalStrength}")
        return f.joinToString(", ")
    }

    private fun describeLink(lp: LinkProperties?): String {
        if (lp == null) return "нет данных"
        val v4 = lp.linkAddresses.count { it.address is java.net.Inet4Address }
        val v6 = lp.linkAddresses.count { it.address is java.net.Inet6Address && !it.address.isLinkLocalAddress }
        val dns = lp.dnsServers.size
        val priv = if (Build.VERSION.SDK_INT >= 28) (if (lp.isPrivateDnsActive) ", частный DNS${lp.privateDnsServerName?.let { " ($it)" } ?: ""}" else "") else ""
        return "интерфейс ${lp.interfaceName ?: "?"}, IPv4: $v4, IPv6: $v6, DNS-серверов: $dns$priv, MTU ${if (Build.VERSION.SDK_INT >= 29) lp.mtu else "?"}"
    }

    private fun startWatchers(ctx: Context) {
        val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(n: Network) = add("сеть", "появилась сеть $n: ${describeCaps(cm.getNetworkCapabilities(n))}")
            override fun onLost(n: Network) = add("сеть", "пропала сеть $n")
            override fun onCapabilitiesChanged(n: Network, c: NetworkCapabilities) = add("сеть", "сеть $n: ${describeCaps(c)}")
            override fun onLinkPropertiesChanged(n: Network, lp: LinkProperties) = add("сеть", "сеть $n: ${describeLink(lp)}")
            override fun onBlockedStatusChanged(n: Network, blocked: Boolean) = add("сеть", "сеть $n: ${if (blocked) "ЗАБЛОКИРОВАНА для приложения (энергосбережение / экономия трафика / VPN)" else "доступна приложению"}")
        }
        try {
            // Default request is non-VPN only; drop that to see the VPN too.
            val req = NetworkRequest.Builder().removeCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN).build()
            cm.registerNetworkCallback(req, cb)
            netCb = cb
        } catch (e: Exception) {
            add("сеть", "не удалось следить за сетями: ${e.javaClass.simpleName}: ${e.message}")
        }
        if (Build.VERSION.SDK_INT >= 24) {
            try {
                cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
                    override fun onAvailable(n: Network) = add("сеть", "сеть по умолчанию: $n (${transports(cm.getNetworkCapabilities(n))})")
                    override fun onLost(n: Network) = add("сеть", "сеть по умолчанию $n пропала")
                }.also { defaultCb = it })
            } catch (_: Exception) {}
        }
        val r = object : BroadcastReceiver() {
            override fun onReceive(c: Context, i: Intent) {
                val a = i.action ?: return
                when (a) {
                    Intent.ACTION_BATTERY_CHANGED -> {
                        val level = i.getIntExtra(BatteryManager.EXTRA_LEVEL, -1)
                        val scale = i.getIntExtra(BatteryManager.EXTRA_SCALE, 100)
                        val pct = if (level >= 0 && scale > 0) level * 100 / scale else -1
                        // Every percent is noise: log only each 5 % step and plug changes.
                        val plugged = i.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0) != 0
                        val key = "${pct / 5}|$plugged"
                        if (key != lastBattery) {
                            lastBattery = key
                            add("питание", "батарея $pct%${if (plugged) ", заряжается" else ""}, температура ${i.getIntExtra(BatteryManager.EXTRA_TEMPERATURE, 0) / 10.0}°C")
                        }
                    }
                    PowerManager.ACTION_POWER_SAVE_MODE_CHANGED -> add("питание", "режим энергосбережения: ${if (pm(c).isPowerSaveMode) "ВКЛ" else "выкл"}")
                    PowerManager.ACTION_DEVICE_IDLE_MODE_CHANGED -> add("питание", "глубокий сон (Doze): ${if (pm(c).isDeviceIdleMode) "ВКЛ — сеть для приложений ограничена" else "выкл"}")
                    Intent.ACTION_SCREEN_ON -> add("экран", "экран включён")
                    Intent.ACTION_SCREEN_OFF -> add("экран", "экран выключен")
                    Intent.ACTION_USER_PRESENT -> add("экран", "телефон разблокирован")
                    Intent.ACTION_AIRPLANE_MODE_CHANGED -> add("сеть", "режим полёта: ${if (i.getBooleanExtra("state", false)) "ВКЛ" else "выкл"}")
                    ConnectivityManager.ACTION_RESTRICT_BACKGROUND_CHANGED -> add("сеть", "экономия трафика: ${dataSaver(c)}")
                    Intent.ACTION_TIME_CHANGED -> add("система", "время на телефоне изменено вручную")
                    Intent.ACTION_TIMEZONE_CHANGED -> add("система", "сменился часовой пояс")
                }
            }
        }
        val f = IntentFilter().apply {
            addAction(Intent.ACTION_BATTERY_CHANGED)
            addAction(PowerManager.ACTION_POWER_SAVE_MODE_CHANGED)
            if (Build.VERSION.SDK_INT >= 23) addAction(PowerManager.ACTION_DEVICE_IDLE_MODE_CHANGED)
            addAction(Intent.ACTION_SCREEN_ON)
            addAction(Intent.ACTION_SCREEN_OFF)
            addAction(Intent.ACTION_USER_PRESENT)
            addAction(Intent.ACTION_AIRPLANE_MODE_CHANGED)
            if (Build.VERSION.SDK_INT >= 24) addAction(ConnectivityManager.ACTION_RESTRICT_BACKGROUND_CHANGED)
            addAction(Intent.ACTION_TIME_CHANGED)
            addAction(Intent.ACTION_TIMEZONE_CHANGED)
        }
        try {
            if (Build.VERSION.SDK_INT >= 33) ctx.registerReceiver(r, f, Context.RECEIVER_NOT_EXPORTED) else ctx.registerReceiver(r, f)
            receiver = r
        } catch (e: Exception) {
            add("питание", "не удалось следить за питанием: ${e.javaClass.simpleName}")
        }
    }

    private var defaultCb: ConnectivityManager.NetworkCallback? = null
    @Volatile private var lastBattery: String? = null

    private fun stopWatchers(ctx: Context) {
        val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        netCb?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        defaultCb?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        receiver?.let { runCatching { ctx.unregisterReceiver(it) } }
        netCb = null; defaultCb = null; receiver = null; lastBattery = null
    }

    private fun pm(c: Context) = c.getSystemService(Context.POWER_SERVICE) as PowerManager

    private fun dataSaver(c: Context): String = try {
        when ((c.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager).restrictBackgroundStatus) {
            ConnectivityManager.RESTRICT_BACKGROUND_STATUS_ENABLED -> "ВКЛ — фоновые данные приложения ограничены"
            ConnectivityManager.RESTRICT_BACKGROUND_STATUS_WHITELISTED -> "вкл, но приложение в исключениях"
            else -> "выкл"
        }
    } catch (_: Exception) { "?" }

    // ---------------------------------------------------------------- snapshot

    /** Everything about the phone's state that can make the app misbehave. */
    fun env(ctx: Context): List<String> {
        val out = mutableListOf<String>()
        val pm = pm(ctx)
        try {
            val bm = ctx.getSystemService(Context.BATTERY_SERVICE) as BatteryManager
            val pct = bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY)
            val charging = if (Build.VERSION.SDK_INT >= 23) bm.isCharging else null
            val ignoring = pm.isIgnoringBatteryOptimizations(ctx.packageName)
            var s = "питание: батарея $pct%${if (charging == true) ", заряжается" else ""}; энергосбережение ${if (pm.isPowerSaveMode) "ВКЛ" else "выкл"}; " +
                "Doze ${if (pm.isDeviceIdleMode) "ВКЛ" else "выкл"}; экран ${if (pm.isInteractive) "вкл" else "выкл"}; " +
                "оптимизация батареи для приложения ${if (ignoring) "отключена (хорошо)" else "ВКЛЮЧЕНА — Android может усыплять службу"}"
            if (Build.VERSION.SDK_INT >= 29) s += "; нагрев ${pm.currentThermalStatus}"
            if (Build.VERSION.SDK_INT >= 28) s += "; геолокация при энергосбережении ${pm.locationPowerSaveMode}"
            out.add(s)
        } catch (e: Exception) { out.add("питание: ${e.javaClass.simpleName}") }
        try {
            var s = "фон: экономия трафика ${dataSaver(ctx)}"
            if (Build.VERSION.SDK_INT >= 28) {
                val am = ctx.getSystemService(Context.ACTIVITY_SERVICE) as ActivityManager
                s += "; фоновая работа ${if (am.isBackgroundRestricted) "ОГРАНИЧЕНА пользователем" else "разрешена"}"
                val bucket = (ctx.getSystemService(Context.USAGE_STATS_SERVICE) as UsageStatsManager).appStandbyBucket
                s += "; группа ожидания ${when (bucket) { 5 -> "освобождено"; 10 -> "активно"; 20 -> "рабочий набор"; 30 -> "частое"; 40 -> "редкое"; 45 -> "ОГРАНИЧЕНО"; else -> bucket.toString() }}"
            }
            s += "; режим полёта ${if (Settings.Global.getInt(ctx.contentResolver, Settings.Global.AIRPLANE_MODE_ON, 0) == 1) "ВКЛ" else "выкл"}"
            val pdns = runCatching { Settings.Global.getString(ctx.contentResolver, "private_dns_mode") }.getOrNull()
            if (pdns != null) s += "; частный DNS: $pdns"
            out.add(s)
        } catch (e: Exception) { out.add("фон: ${e.javaClass.simpleName}") }
        try {
            val am = ctx.getSystemService(Context.ACTIVITY_SERVICE) as ActivityManager
            val mi = ActivityManager.MemoryInfo().also { am.getMemoryInfo(it) }
            val rt = Runtime.getRuntime()
            out.add("память: свободно ${mi.availMem / 1048576} из ${mi.totalMem / 1048576} МБ${if (mi.lowMemory) " — МАЛО ПАМЯТИ" else ""}; " +
                "приложение ${(rt.totalMemory() - rt.freeMemory()) / 1048576} МБ (Java)")
        } catch (_: Exception) {}
        try {
            val tm = ctx.getSystemService(Context.TELEPHONY_SERVICE) as TelephonyManager
            var s = "SIM: состояние ${when (tm.simState) { TelephonyManager.SIM_STATE_READY -> "готова"; TelephonyManager.SIM_STATE_ABSENT -> "НЕТ SIM"; else -> tm.simState.toString() }}; " +
                "оператор сети «${tm.networkOperatorName}» (${tm.networkOperator}), SIM «${tm.simOperatorName}» (${tm.simOperator}); " +
                "роуминг ${if (tm.isNetworkRoaming) "ДА" else "нет"}; передача данных ${when (tm.dataState) { TelephonyManager.DATA_CONNECTED -> "подключена"; TelephonyManager.DATA_CONNECTING -> "подключается"; TelephonyManager.DATA_SUSPENDED -> "ПРИОСТАНОВЛЕНА"; else -> "отключена" }}"
            if (Build.VERSION.SDK_INT >= 26) s += "; мобильные данные ${runCatching { if (tm.isDataEnabled) "вкл" else "ВЫКЛ" }.getOrDefault("?")}"
            if (Build.VERSION.SDK_INT >= 28) runCatching { tm.signalStrength?.level }.getOrNull()?.let { s += "; уровень сигнала $it/4" }
            out.add(s)
        } catch (e: Exception) { out.add("SIM: ${e.javaClass.simpleName}") }
        try {
            val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
            val nets = cm.allNetworks
            out.add("сети (${nets.size}): " + nets.joinToString("; ") { n -> "$n ${describeCaps(cm.getNetworkCapabilities(n))}" }.ifEmpty { "нет" })
            cm.activeNetwork?.let { out.add("сеть по умолчанию: $it, ${describeLink(cm.getLinkProperties(it))}") }
            cm.boundNetworkForProcess.let { out.add("процесс привязан к сети: ${it ?: "нет (по умолчанию)"}") }
        } catch (e: Exception) { out.add("сети: ${e.javaClass.simpleName}") }
        return out
    }

    // ------------------------------------------------- crashes and process exits

    private val fmt = SimpleDateFormat("dd.MM HH:mm:ss", Locale.US)

    fun installCrashHandler(ctx: Context) {
        val app = ctx.applicationContext
        val prev = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { t, e ->
            try {
                val sw = java.io.StringWriter()
                e.printStackTrace(java.io.PrintWriter(sw))
                app.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
                    .putString("last_crash", "${fmt.format(Date())} поток «${t.name}»: ${sw.toString().take(3500)}")
                    .commit()
            } catch (_: Throwable) {}
            prev?.uncaughtException(t, e)
        }
    }

    /** Lines about what ended the app since the last time this was asked. */
    fun pastExits(ctx: Context): List<String> {
        val out = mutableListOf<String>()
        val p = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        p.getString("last_crash", null)?.let {
            out.add("СБОЙ приложения (Java/Kotlin) в прошлый раз: $it")
            p.edit().remove("last_crash").apply()
        }
        if (Build.VERSION.SDK_INT >= 30) {
            try {
                val am = ctx.getSystemService(Context.ACTIVITY_SERVICE) as ActivityManager
                val seen = p.getLong("exit_seen", 0L)
                val list = am.getHistoricalProcessExitReasons(ctx.packageName, 0, 15).filter { it.timestamp > seen }.sortedBy { it.timestamp }
                for (x in list) {
                    val why = when (x.reason) {
                        ApplicationExitInfo.REASON_ANR -> "ЗАВИСЛО (ANR)"
                        ApplicationExitInfo.REASON_CRASH -> "СБОЙ (Java)"
                        ApplicationExitInfo.REASON_CRASH_NATIVE -> "СБОЙ (нативный код)"
                        ApplicationExitInfo.REASON_DEPENDENCY_DIED -> "умер связанный процесс"
                        ApplicationExitInfo.REASON_EXCESSIVE_RESOURCE_USAGE -> "убито за чрезмерный расход ресурсов"
                        ApplicationExitInfo.REASON_EXIT_SELF -> "завершилось само"
                        ApplicationExitInfo.REASON_INITIALIZATION_FAILURE -> "ошибка запуска"
                        ApplicationExitInfo.REASON_LOW_MEMORY -> "УБИТО СИСТЕМОЙ: мало памяти"
                        ApplicationExitInfo.REASON_OTHER -> "убито системой (другое)"
                        ApplicationExitInfo.REASON_PERMISSION_CHANGE -> "изменились разрешения"
                        ApplicationExitInfo.REASON_SIGNALED -> "убито сигналом (часто — оптимизатор батареи производителя)"
                        ApplicationExitInfo.REASON_USER_REQUESTED -> "остановлено пользователем (принудительно / смахнули с очисткой)"
                        ApplicationExitInfo.REASON_USER_STOPPED -> "пользователь остановил приложение"
                        else -> if (Build.VERSION.SDK_INT >= 34 && x.reason == ApplicationExitInfo.REASON_FREEZER) "заморожено системой" else "причина ${x.reason}"
                    }
                    val imp = when (x.importance) {
                        ActivityManager.RunningAppProcessInfo.IMPORTANCE_FOREGROUND -> "на экране"
                        ActivityManager.RunningAppProcessInfo.IMPORTANCE_FOREGROUND_SERVICE -> "работала служба"
                        ActivityManager.RunningAppProcessInfo.IMPORTANCE_CACHED -> "в фоне"
                        else -> "важность ${x.importance}"
                    }
                    out.add("процесс завершился ${fmt.format(Date(x.timestamp))}: $why ($imp)" +
                        (x.description?.let { "; «${it.take(300)}»" } ?: "") + "; память ${x.pss / 1024} МБ")
                }
                list.lastOrNull()?.let { p.edit().putLong("exit_seen", it.timestamp).apply() }
                if (seen == 0L && list.isEmpty()) p.edit().putLong("exit_seen", System.currentTimeMillis()).apply()
            } catch (e: Exception) {
                out.add("причины завершения недоступны: ${e.javaClass.simpleName}")
            }
        }
        return out
    }
}
