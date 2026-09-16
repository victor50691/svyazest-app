package com.svyazest.svyazest_app

import android.app.Application
import com.pravera.flutter_foreground_task.FlutterForegroundTaskLifecycleListener
import com.pravera.flutter_foreground_task.FlutterForegroundTaskPlugin
import com.pravera.flutter_foreground_task.FlutterForegroundTaskStarter
import io.flutter.embedding.engine.FlutterEngine

/** Attaches NativePlugin to the foreground service's engine. Lives in the
 *  Application (not MainActivity) so it also works when the service is
 *  restarted on boot with no activity around. */
class App : Application() {
    override fun onCreate() {
        super.onCreate()
        FlutterForegroundTaskPlugin.addTaskLifecycleListener(object : FlutterForegroundTaskLifecycleListener {
            override fun onEngineCreate(flutterEngine: FlutterEngine?) {
                flutterEngine?.plugins?.add(NativePlugin())
            }
            override fun onTaskStart(starter: FlutterForegroundTaskStarter) {}
            override fun onTaskRepeatEvent() {}
            override fun onTaskDestroy() {}
            override fun onEngineWillDestroy() {}
        })
    }
}
