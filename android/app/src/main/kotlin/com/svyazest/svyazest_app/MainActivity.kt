package com.svyazest.svyazest_app

import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine

class MainActivity : FlutterActivity() {
    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        // Same plugin the service engine gets from App.kt.
        flutterEngine.plugins.add(NativePlugin())
    }
}
