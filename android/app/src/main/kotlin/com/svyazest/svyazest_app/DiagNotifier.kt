package com.svyazest.svyazest_app

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.widget.Toast
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat

/**
 * «Разрешить сбор логов?» -- the request from support (admin panel →
 * Партнёры → «Логи»). The notification stays until the partner answers:
 * it is ongoing, and if it is swiped away anyway (Android 14+ allows that
 * for ongoing notifications) DiagReceiver posts it again.
 *
 * The answer from the notification buttons is only stored here
 * (SharedPreferences "svyazest_diag"); Dart sends it to the server from the
 * service loop or the app (lib/services/diag_log.dart, DiagConsent) and
 * clears it once delivered.
 */
object DiagNotifier {
    private const val CHANNEL_ID = "diag_requests"
    private const val NOTIF_ID = 7301
    private const val PREFS = "svyazest_diag"
    const val ACTION_ACCEPT = "com.svyazest.svyazest_app.DIAG_ACCEPT"
    const val ACTION_DECLINE = "com.svyazest.svyazest_app.DIAG_DECLINE"
    const val ACTION_DISMISSED = "com.svyazest.svyazest_app.DIAG_DISMISSED"
    const val EXTRA_ID = "diag_id"

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    /** Ask about request [id] -- unless this phone already answered it and
     *  the answer is still on its way to the server. */
    fun show(ctx: Context, id: Int, reason: String = "") {
        val p = prefs(ctx)
        if (p.getInt("decision_id", -1) == id) return
        // The reason is kept for re-posting after a swipe (DiagReceiver).
        p.edit().putInt("pending_id", id).putString("reason", reason).apply()
        post(ctx, id)
    }

    fun cancel(ctx: Context) {
        prefs(ctx).edit().putInt("pending_id", -1).apply()
        NotificationManagerCompat.from(ctx).cancel(NOTIF_ID)
    }

    fun pendingId(ctx: Context): Int = prefs(ctx).getInt("pending_id", -1)

    /** The answer given on the notification and not delivered yet. */
    fun peekDecision(ctx: Context): Map<String, Any>? {
        val p = prefs(ctx)
        val id = p.getInt("decision_id", -1)
        if (id < 0) return null
        return mapOf("id" to id, "accept" to (p.getString("decision", "") == "accept"))
    }

    fun clearDecision(ctx: Context, id: Int) {
        val p = prefs(ctx)
        if (p.getInt("decision_id", -1) == id) p.edit().remove("decision_id").remove("decision").apply()
    }

    fun decide(ctx: Context, id: Int, accept: Boolean) {
        prefs(ctx).edit()
            .putInt("decision_id", id)
            .putString("decision", if (accept) "accept" else "decline")
            .putInt("pending_id", -1)
            .apply()
        NotificationManagerCompat.from(ctx).cancel(NOTIF_ID)
    }

    private fun ensureChannel(ctx: Context) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (nm.getNotificationChannel(CHANNEL_ID) != null) return
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL_ID, "Запросы поддержки", NotificationManager.IMPORTANCE_HIGH).apply {
                description = "Просьба поддержки разрешить сбор логов для диагностики"
            }
        )
    }

    private fun broadcast(ctx: Context, action: String, id: Int, code: Int): PendingIntent =
        PendingIntent.getBroadcast(
            ctx, code,
            Intent(ctx, DiagReceiver::class.java).setAction(action).putExtra(EXTRA_ID, id),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )

    fun post(ctx: Context, id: Int) {
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(ctx, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return
        ensureChannel(ctx)
        val launch = ctx.packageManager.getLaunchIntentForPackage(ctx.packageName)
        val open = launch?.let {
            PendingIntent.getActivity(ctx, 0, it, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        }
        val reason = prefs(ctx).getString("reason", "").orEmpty().trim()
        val text = (if (reason.isNotEmpty()) "Причина запроса: $reason\n\n" else "") +
            "Чтобы разобраться, почему приложение «Связь Есть?» работает не так, как задумано, " +
            "нам нужны технические логи с этого устройства. " +
            "Сбор отключит поддержка, когда закончит диагностику; остановить его самому можно в любой момент в приложении."
        val n = NotificationCompat.Builder(ctx, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_diag)
            .setContentTitle("Разрешите диагностический сбор логов")
            .setContentText(if (reason.isNotEmpty()) "Причина: $reason" else "Нам нужны технические логи для диагностики приложения")
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setCategory(NotificationCompat.CATEGORY_REMINDER)
            .setOngoing(true)
            .setAutoCancel(false)
            .setOnlyAlertOnce(true)
            .setDeleteIntent(broadcast(ctx, ACTION_DISMISSED, id, 3))
            .addAction(0, "Разрешить", broadcast(ctx, ACTION_ACCEPT, id, 1))
            .addAction(0, "Отклонить", broadcast(ctx, ACTION_DECLINE, id, 2))
        if (open != null) n.setContentIntent(open)
        try {
            NotificationManagerCompat.from(ctx).notify(NOTIF_ID, n.build())
        } catch (_: SecurityException) {
        }
    }
}

class DiagReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        val id = intent.getIntExtra(DiagNotifier.EXTRA_ID, -1)
        if (id < 0) return
        when (intent.action) {
            DiagNotifier.ACTION_ACCEPT, DiagNotifier.ACTION_DECLINE -> {
                val accept = intent.action == DiagNotifier.ACTION_ACCEPT
                DiagNotifier.decide(ctx, id, accept)
                Toast.makeText(ctx, if (accept) "Сбор логов разрешён" else "Сбор логов отклонён", Toast.LENGTH_SHORT).show()
            }
            // Swiped away without an answer: it comes back until answered.
            DiagNotifier.ACTION_DISMISSED -> if (DiagNotifier.pendingId(ctx) == id) DiagNotifier.post(ctx, id)
        }
    }
}
