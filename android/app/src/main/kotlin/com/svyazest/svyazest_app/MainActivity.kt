package com.svyazest.svyazest_app

import android.content.Intent
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine

class MainActivity : FlutterActivity() {
    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        // Same plugin the service engine gets from App.kt.
        flutterEngine.plugins.add(NativePlugin())
        // GPS check (permission + one fix) needs an Activity: UI engine only.
        flutterEngine.plugins.add(GpsPlugin())
        // «Разрешить» on the GPS notification opened the app.
        GpsPlugin.captureIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        GpsPlugin.captureIntent(intent)
    }
}
