package com.svyazest.svyazest_app

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.app.AppOpsManager
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.location.GnssStatus
import android.location.Location
import android.location.LocationListener
import android.location.LocationManager
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.provider.Settings
import android.widget.Toast
import androidx.core.app.ActivityCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import androidx.core.location.LocationManagerCompat
import io.flutter.embedding.engine.plugins.FlutterPlugin
import io.flutter.embedding.engine.plugins.activity.ActivityAware
import io.flutter.embedding.engine.plugins.activity.ActivityPluginBinding
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import io.flutter.plugin.common.PluginRegistry
import com.google.android.gms.common.ConnectionResult
import com.google.android.gms.common.GoogleApiAvailability
import com.google.android.gms.common.api.ResolvableApiException
import com.google.android.gms.location.LocationCallback
import com.google.android.gms.location.LocationResult
import com.google.android.gms.location.LocationServices
import com.google.android.gms.location.LocationSettingsRequest
import com.google.android.gms.location.Priority

/**
 * «Подтвердить местоположение» -- a one-time GPS check support asks for
 * (admin panel → Партнёры → GPS) when the region a partner claimed looks
 * doubtful. Nothing here runs unless the partner agreed; there is no
 * background tracking: one fix, then the listeners are removed.
 *
 * UI engine only (MainActivity): runtime permission needs an Activity. The
 * notification itself (GpsNotifier) is also reachable from the service
 * engine through NativePlugin.
 */
class GpsPlugin : FlutterPlugin, ActivityAware, MethodChannel.MethodCallHandler,
    PluginRegistry.RequestPermissionsResultListener, PluginRegistry.ActivityResultListener {

    companion object {
        const val CHANNEL = "com.svyazest.svyazest_app/gps"
        private const val PERM_REQUEST = 7411
        private const val ACCURACY_REQUEST = 7412
        // «Разрешить» on the notification opens the app with these extras;
        // Dart picks them up with takeLaunchAction.
        const val EXTRA_ACTION = "gps_action"
        const val EXTRA_ID = "gps_id"
        @Volatile var launchAction: Pair<Int, String>? = null

        fun captureIntent(intent: Intent?) {
            val id = intent?.getIntExtra(EXTRA_ID, -1) ?: -1
            val action = intent?.getStringExtra(EXTRA_ACTION)
            if (id >= 0 && action != null) {
                launchAction = id to action
                intent.removeExtra(EXTRA_ID)
                intent.removeExtra(EXTRA_ACTION)
            }
        }
    }

    private lateinit var context: Context
    private var channel: MethodChannel? = null
    private var activity: Activity? = null
    private var binding: ActivityPluginBinding? = null
    private var permResult: MethodChannel.Result? = null
    private var accuracyResult: MethodChannel.Result? = null
    private val main = Handler(Looper.getMainLooper())

    override fun onAttachedToEngine(b: FlutterPlugin.FlutterPluginBinding) {
        context = b.applicationContext
        channel = MethodChannel(b.binaryMessenger, CHANNEL).also { it.setMethodCallHandler(this) }
    }

    override fun onDetachedFromEngine(b: FlutterPlugin.FlutterPluginBinding) {
        channel?.setMethodCallHandler(null)
    }

    override fun onAttachedToActivity(b: ActivityPluginBinding) {
        activity = b.activity; binding = b; b.addRequestPermissionsResultListener(this); b.addActivityResultListener(this)
    }
    override fun onDetachedFromActivityForConfigChanges() = onDetachedFromActivity()
    override fun onReattachedToActivityForConfigChanges(b: ActivityPluginBinding) = onAttachedToActivity(b)
    override fun onDetachedFromActivity() {
        binding?.removeRequestPermissionsResultListener(this); binding?.removeActivityResultListener(this); binding = null; activity = null
    }

    private fun hasPermission() =
        ContextCompat.checkSelfPermission(context, Manifest.permission.ACCESS_FINE_LOCATION) == PackageManager.PERMISSION_GRANTED

    override fun onMethodCall(call: MethodCall, result: MethodChannel.Result) {
        when (call.method) {
            "takeLaunchAction" -> {
                val a = launchAction; launchAction = null
                result.success(a?.let { mapOf("id" to it.first, "action" to it.second) })
            }
            "hasPermission" -> result.success(hasPermission())
            "requestPermission" -> {
                val act = activity
                if (hasPermission()) { result.success(true); return }
                if (act == null) { result.success(false); return }
                permResult = result
                ActivityCompat.requestPermissions(
                    act, arrayOf(Manifest.permission.ACCESS_FINE_LOCATION, Manifest.permission.ACCESS_COARSE_LOCATION), PERM_REQUEST)
            }
            "locationEnabled" -> result.success(locationEnabled())
            "openLocationSettings" -> {
                try {
                    context.startActivity(Intent(Settings.ACTION_LOCATION_SOURCE_SETTINGS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                    result.success(true)
                } catch (_: Exception) { result.success(false) }
            }
            "openAppSettings" -> {
                try {
                    context.startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                        .setData(android.net.Uri.parse("package:" + context.packageName)).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                    result.success(true)
                } catch (_: Exception) { result.success(false) }
            }
            "fix" -> fix(call.argument<Int>("timeoutMs") ?: 90000, result)
            "sources" -> result.success(sources())
            // After the check (1.15.0): the app needs location for nothing else,
            // so it gives the permission back (Android 13+; takes effect when
            // the app's process ends -- the next check asks again).
            "releasePermission" -> {
                if (Build.VERSION.SDK_INT >= 33) {
                    try {
                        context.revokeSelfPermissionsOnKill(listOf(Manifest.permission.ACCESS_FINE_LOCATION, Manifest.permission.ACCESS_COARSE_LOCATION))
                        NativeLog.add("gps", "доступ к геолокации отдан обратно (снимется, когда процесс приложения завершится)")
                        result.success(true)
                    } catch (e: Exception) { result.success(false) }
                } else result.success(false)
            }
            "ensureAccuracy" -> ensureAccuracy(result)
            else -> result.notImplemented()
        }
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?): Boolean {
        if (requestCode != ACCURACY_REQUEST) return false
        NativeLog.add("gps", if (resultCode == Activity.RESULT_OK) "партнёр включил точность определения" else "партнёр отказался включить точность определения")
        accuracyResult?.success(mapOf("state" to if (resultCode == Activity.RESULT_OK) "enabled" else "declined", "gms" to true))
        accuracyResult = null
        return true
    }

    private fun gmsAvailable(): Boolean = try {
        GoogleApiAvailability.getInstance().isGooglePlayServicesAvailable(context) == ConnectionResult.SUCCESS
    } catch (_: Throwable) { false }

    /** Can this phone place itself without satellites (Wi-Fi / cell towers)? */
    private fun sources(): Map<String, Any?> {
        val lm = context.getSystemService(Context.LOCATION_SERVICE) as LocationManager
        val network = try { lm.allProviders.contains(LocationManager.NETWORK_PROVIDER) && lm.isProviderEnabled(LocationManager.NETWORK_PROVIDER) } catch (_: Exception) { false }
        return mapOf("network" to network, "gms" to gmsAvailable())
    }

    /**
     * 1.13.0: ask Google Play services for high-accuracy location settings.
     * If «Точность определения местоположения» (Wi-Fi / towers) is off, the
     * system shows its own one-tap dialog to turn it on. Without it the
     * phone only has satellites -- none indoors (2026-11-21: 0 satellites in
     * 90 s, providers passive,fused,gps). Result state: already | enabled |
     * declined | unavailable (no Google services / no screen).
     */
    private fun ensureAccuracy(result: MethodChannel.Result) {
        if (!gmsAvailable()) { result.success(mapOf("state" to "unavailable", "gms" to false)); return }
        val act = activity
        val req = com.google.android.gms.location.LocationRequest.Builder(Priority.PRIORITY_HIGH_ACCURACY, 1000L).build()
        val settings = LocationSettingsRequest.Builder().addLocationRequest(req).setAlwaysShow(true).build()
        try {
            LocationServices.getSettingsClient(context).checkLocationSettings(settings)
                .addOnSuccessListener { NativeLog.add("gps", "точность определения уже включена"); result.success(mapOf("state" to "already", "gms" to true)) }
                .addOnFailureListener { e ->
                    if (e is ResolvableApiException && act != null) {
                        try {
                            accuracyResult = result
                            e.startResolutionForResult(act, ACCURACY_REQUEST)
                        } catch (_: Exception) {
                            accuracyResult = null
                            result.success(mapOf("state" to "unavailable", "gms" to true))
                        }
                    } else {
                        result.success(mapOf("state" to "unavailable", "gms" to true))
                    }
                }
        } catch (_: Throwable) {
            result.success(mapOf("state" to "unavailable", "gms" to true))
        }
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray): Boolean {
        if (requestCode != PERM_REQUEST) return false
        permResult?.success(hasPermission()); permResult = null
        return true
    }

    private fun locationEnabled(): Boolean {
        val lm = context.getSystemService(Context.LOCATION_SERVICE) as LocationManager
        return LocationManagerCompat.isLocationEnabled(lm)
    }

    /** Signals that the device can fake its location, independent of the fix. */
    private fun environment(): Map<String, Any?> {
        val cr = context.contentResolver
        val devOptions = Settings.Global.getInt(cr, Settings.Global.DEVELOPMENT_SETTINGS_ENABLED, 0) == 1
        // Apps that may act as the mock location provider, and whether one is
        // actually selected in Developer options (AppOps «mock_location»).
        val mockApps = mutableListOf<String>()
        var mockSelected: String? = null
        try {
            val pm = context.packageManager
            val ops = context.getSystemService(Context.APP_OPS_SERVICE) as AppOpsManager
            @Suppress("DEPRECATION")
            val pkgs = pm.getPackagesHoldingPermissions(arrayOf("android.permission.ACCESS_MOCK_LOCATION"), 0)
            for (p in pkgs) {
                if (p.packageName == context.packageName) continue
                mockApps.add(p.packageName)
                val uid = p.applicationInfo?.uid ?: continue
                val mode = if (Build.VERSION.SDK_INT >= 29)
                    ops.unsafeCheckOpNoThrow(AppOpsManager.OPSTR_MOCK_LOCATION, uid, p.packageName)
                else @Suppress("DEPRECATION") ops.checkOpNoThrow(AppOpsManager.OPSTR_MOCK_LOCATION, uid, p.packageName)
                if (mode == AppOpsManager.MODE_ALLOWED) mockSelected = p.packageName
            }
        } catch (_: Exception) {}
        return mapOf("dev_options" to devOptions, "mock_apps" to mockApps.take(10), "mock_app_selected" to mockSelected)
    }

    /** Freshest last-known location of any provider, if not older than [maxAgeMs]. */
    @SuppressLint("MissingPermission")
    private fun lastKnown(lm: LocationManager, maxAgeMs: Long): Location? {
        var best: Location? = null
        for (p in lm.allProviders) {
            val l = try { lm.getLastKnownLocation(p) } catch (_: Exception) { null } ?: continue
            val age = (SystemClock.elapsedRealtimeNanos() - l.elapsedRealtimeNanos) / 1_000_000
            if (age > maxAgeMs) continue
            if (best == null || l.elapsedRealtimeNanos > best.elapsedRealtimeNanos) best = l
        }
        return best
    }

    /**
     * One fix. 2026-11-21: a phone (KCELL) gave nothing in 60 s -- no
     * satellite seen, nothing from the network provider. Now: the fused
     * provider too (Android 12+, Google location services), 90 s, and if no
     * fresh fix came at all, the freshest last-known location of the last
     * 30 min (source = last_known, its age goes to the server, which flags
     * it). The result also says which providers were on and whether the
     * GNSS engine started at all, so a failure can be explained.
     */
    @SuppressLint("MissingPermission")
    private fun fix(timeoutMs: Int, result: MethodChannel.Result) {
        if (!hasPermission()) { result.success(mapOf("error" to "permission_denied")); return }
        if (!locationEnabled()) { result.success(mapOf("error" to "location_off")); return }
        val lm = context.getSystemService(Context.LOCATION_SERVICE) as LocationManager
        var best: Location? = null
        var satUsed = 0
        var satSeen = 0
        var gnssStarted = false
        var done = false
        var gnssCb: GnssStatus.Callback? = null
        val requested = mutableListOf<String>()
        val errors = mutableListOf<String>()
        val enabled = lm.allProviders.filter { p -> try { lm.isProviderEnabled(p) } catch (_: Exception) { false } }
        lateinit var listener: LocationListener
        var gmsCb: LocationCallback? = null
        val gmsClient = if (gmsAvailable()) try { LocationServices.getFusedLocationProviderClient(context) } catch (_: Throwable) { null } else null
        lateinit var finish: () -> Unit
        val timeout = Runnable { finish() }
        fun describe(l: Location, source: String): Map<String, Any?> {
            val mock = if (Build.VERSION.SDK_INT >= 31) l.isMock else @Suppress("DEPRECATION") l.isFromMockProvider
            return mapOf(
                "lat" to l.latitude, "lon" to l.longitude, "accuracy" to l.accuracy.toDouble(),
                "provider" to l.provider, "mock" to mock, "source" to source,
                "altitude" to (if (l.hasAltitude()) l.altitude else null),
                "speed" to (if (l.hasSpeed()) l.speed.toDouble() else null),
                "age_ms" to (SystemClock.elapsedRealtimeNanos() - l.elapsedRealtimeNanos) / 1_000_000,
                "time" to l.time,
            )
        }
        finish = {
            if (!done) {
                done = true
                main.removeCallbacks(timeout)
                try { lm.removeUpdates(listener) } catch (_: Exception) {}
                gmsCb?.let { cb -> try { gmsClient?.removeLocationUpdates(cb) } catch (_: Throwable) {} }
                gnssCb?.let { try { lm.unregisterGnssStatusCallback(it) } catch (_: Exception) {} }
                val diag = mapOf(
                    "satellites_used" to satUsed, "satellites_seen" to satSeen, "gnss_started" to gnssStarted,
                    "providers_enabled" to enabled.joinToString(","), "providers_requested" to requested.joinToString(","),
                    "gms" to (gmsClient != null),
                    "provider_errors" to errors.joinToString("; ").take(200),
                )
                val env = environment()
                val l = best
                NativeLog.add("gps", "поиск завершён: ${if (l != null) "точка ${l.provider} ±${l.accuracy.toInt()} м" else "точки нет"}; спутников видно $satSeen, в расчёте $satUsed; " +
                    "GPS запускался: $gnssStarted; источники: ${enabled.joinToString(",")}; запрошены: ${requested.joinToString(",")}${if (errors.isEmpty()) "" else "; ошибки: ${errors.joinToString("; ")}"}")
                if (l != null) {
                    result.success(env + diag + describe(l, "fresh"))
                } else {
                    val old = lastKnown(lm, 30 * 60 * 1000L)
                    if (old != null) result.success(env + diag + describe(old, "last_known"))
                    else result.success(env + diag + mapOf("error" to "no_fix"))
                }
            }
        }
        listener = object : LocationListener {
            override fun onLocationChanged(loc: Location) {
                NativeLog.add("gps", "точка от «${loc.provider}» ±${loc.accuracy.toInt()} м, возраст ${(SystemClock.elapsedRealtimeNanos() - loc.elapsedRealtimeNanos) / 1_000_000} мс")
                val b = best
                val isGps = loc.provider == LocationManager.GPS_PROVIDER
                if (b == null || loc.accuracy < b.accuracy || (isGps && b.provider != LocationManager.GPS_PROVIDER)) best = loc
                // A good satellite fix ends the wait early; so does a decent
                // fused/network one after a while (no point waiting 90 s
                // indoors for satellites that never come).
                if (isGps && loc.accuracy <= 30f) main.post { finish() }
                // Google's fused fix (Wi-Fi / towers / satellites) that is
                // already good: no point waiting for anything better.
                if (!isGps && loc.accuracy <= 50f) main.postDelayed({ finish() }, 3000L)
            }
            @Deprecated("old API") override fun onStatusChanged(p: String?, s: Int, e: Bundle?) {}
            override fun onProviderEnabled(p: String) {}
            override fun onProviderDisabled(p: String) {}
        }
        if (Build.VERSION.SDK_INT >= 24) {
            gnssCb = object : GnssStatus.Callback() {
                override fun onStarted() { gnssStarted = true; NativeLog.add("gps", "GPS-приёмник запущен") }
                override fun onFirstFix(ttffMillis: Int) { NativeLog.add("gps", "первая спутниковая точка через ${ttffMillis} мс") }
                override fun onSatelliteStatusChanged(status: GnssStatus) {
                    gnssStarted = true
                    satSeen = maxOf(satSeen, status.satelliteCount)
                    var used = 0
                    for (i in 0 until status.satelliteCount) if (status.usedInFix(i)) used++
                    satUsed = maxOf(satUsed, used)
                }
            }
            try { lm.registerGnssStatusCallback(gnssCb!!, main) } catch (e: Exception) { gnssCb = null; errors.add("gnss_status: ${e.javaClass.simpleName}") }
        }
        val wanted = mutableListOf(LocationManager.GPS_PROVIDER, LocationManager.NETWORK_PROVIDER)
        if (Build.VERSION.SDK_INT >= 31) wanted.add(LocationManager.FUSED_PROVIDER)
        for (p in wanted) {
            try {
                if (lm.allProviders.contains(p) && lm.isProviderEnabled(p)) {
                    lm.requestLocationUpdates(p, 1000L, 0f, listener, Looper.getMainLooper()); requested.add(p)
                }
            } catch (e: Exception) { errors.add("$p: ${e.javaClass.simpleName}") }
        }
        if (gmsClient != null) {
            try {
                val cb = object : LocationCallback() {
                    override fun onLocationResult(r: LocationResult) { for (l in r.locations) listener.onLocationChanged(l) }
                }
                val req = com.google.android.gms.location.LocationRequest.Builder(Priority.PRIORITY_HIGH_ACCURACY, 1000L).build()
                gmsClient.requestLocationUpdates(req, cb, Looper.getMainLooper())
                gmsCb = cb
                requested.add("google")
            } catch (e: Throwable) { errors.add("google: ${e.javaClass.simpleName}") }
        }
        if (requested.isEmpty()) {
            // No live source: a recent last-known location is still better than nothing.
            val old = lastKnown(lm, 30 * 60 * 1000L)
            done = true
            gnssCb?.let { try { lm.unregisterGnssStatusCallback(it) } catch (_: Exception) {} }
            val diag = mapOf("providers_enabled" to enabled.joinToString(","), "provider_errors" to errors.joinToString("; ").take(200))
            result.success(environment() + diag + (old?.let { describe(it, "last_known") } ?: mapOf("error" to "no_provider")))
            return
        }
        main.postDelayed(timeout, timeoutMs.toLong())
        // Network/fused fix but still no satellites after 45 s: take what we have.
        main.postDelayed({ val b = best; if (!done && b != null && b.accuracy <= 2000f) finish() }, 45_000L)
    }
}

/** «Подтвердить местоположение?» notification -- stays until answered (re-posted
 *  if swiped away). «Разрешить» opens the app, which does the check;
 *  «Отклонить» is stored here and sent to the server by Dart. */
object GpsNotifier {
    private const val CHANNEL_ID = "gps_requests"
    private const val NOTIF_ID = 7302
    private const val PREFS = "svyazest_gps"
    const val ACTION_DECLINE = "com.svyazest.svyazest_app.GPS_DECLINE"
    const val ACTION_DISMISSED = "com.svyazest.svyazest_app.GPS_DISMISSED"

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    fun show(ctx: Context, id: Int, reason: String = "") {
        val p = prefs(ctx)
        if (p.getInt("declined_id", -1) == id) return
        // The reason is kept for re-posting after a swipe (GpsReceiver).
        p.edit().putInt("pending_id", id).putString("reason", reason).apply()
        post(ctx, id)
    }

    fun cancel(ctx: Context) {
        prefs(ctx).edit().putInt("pending_id", -1).apply()
        NotificationManagerCompat.from(ctx).cancel(NOTIF_ID)
    }

    fun pendingId(ctx: Context) = prefs(ctx).getInt("pending_id", -1)
    fun peekDecline(ctx: Context): Int = prefs(ctx).getInt("declined_id", -1)
    fun clearDecline(ctx: Context, id: Int) {
        val p = prefs(ctx); if (p.getInt("declined_id", -1) == id) p.edit().remove("declined_id").apply()
    }
    fun decline(ctx: Context, id: Int) {
        prefs(ctx).edit().putInt("declined_id", id).putInt("pending_id", -1).apply()
        NotificationManagerCompat.from(ctx).cancel(NOTIF_ID)
    }

    fun post(ctx: Context, id: Int) {
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(ctx, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        if (Build.VERSION.SDK_INT >= 26) {
            val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
            if (nm.getNotificationChannel(CHANNEL_ID) == null) {
                nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "Проверка местоположения", NotificationManager.IMPORTANCE_HIGH))
            }
        }
        val launch = ctx.packageManager.getLaunchIntentForPackage(ctx.packageName) ?: return
        val accept = PendingIntent.getActivity(ctx, 11,
            Intent(launch).putExtra(GpsPlugin.EXTRA_ACTION, "accept").putExtra(GpsPlugin.EXTRA_ID, id)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val open = PendingIntent.getActivity(ctx, 10, launch, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        fun bc(action: String, code: Int) = PendingIntent.getBroadcast(ctx, code,
            Intent(ctx, GpsReceiver::class.java).setAction(action).putExtra(GpsPlugin.EXTRA_ID, id),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val reason = prefs(ctx).getString("reason", "").orEmpty().trim()
        val text = (if (reason.isNotEmpty()) "Причина запроса: $reason\n\n" else "") +
            "Нужно один раз подтвердить по GPS, что вы находитесь в указанном в заявке регионе. " +
            "Приложение определит местоположение один раз, без постоянного слежения."
        val n = NotificationCompat.Builder(ctx, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_diag)
            .setContentTitle("Подтвердите ваше местоположение")
            .setContentText(if (reason.isNotEmpty()) "Причина: $reason" else "Нужно один раз проверить регион по GPS")
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setCategory(NotificationCompat.CATEGORY_REMINDER)
            .setOngoing(true).setAutoCancel(false).setOnlyAlertOnce(true)
            .setContentIntent(open)
            .setDeleteIntent(bc(ACTION_DISMISSED, 13))
            .addAction(0, "Разрешить", accept)
            .addAction(0, "Отклонить", bc(ACTION_DECLINE, 12))
        try { NotificationManagerCompat.from(ctx).notify(NOTIF_ID, n.build()) } catch (_: SecurityException) {}
    }
}

class GpsReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        val id = intent.getIntExtra(GpsPlugin.EXTRA_ID, -1)
        if (id < 0) return
        when (intent.action) {
            GpsNotifier.ACTION_DECLINE -> {
                GpsNotifier.decline(ctx, id)
                Toast.makeText(ctx, "Проверка местоположения отклонена", Toast.LENGTH_SHORT).show()
            }
            GpsNotifier.ACTION_DISMISSED -> if (GpsNotifier.pendingId(ctx) == id) GpsNotifier.post(ctx, id)
        }
    }
}
