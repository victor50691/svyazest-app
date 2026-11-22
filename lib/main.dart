import 'dart:ui' show PlatformDispatcher;

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';

import 'screens/main_shell.dart';
import 'screens/login_screen.dart';
import 'screens/root_blocked_screen.dart';
import 'services/api_client.dart';
import 'services/control_http.dart';
import 'services/diag_log.dart';
import 'services/native_bridge.dart';
import 'services/secure_store.dart';
import 'theme.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  // Always portrait (1.16.1); the manifest says so too (MainActivity
  // screenOrientation) -- both, so no screen ever rotates.
  await SystemChrome.setPreferredOrientations([DeviceOrientation.portraitUp]);
  await ApiClient.loadAppVersion();
  // Diagnostics log (only while the partner allowed it, DiagLog): errors
  // anywhere in the UI, app foreground/background, the server route.
  FlutterError.onError = (d) {
    DiagLog.app.logError('интерфейс', d.exception, d.stack);
    FlutterError.presentError(d);
  };
  PlatformDispatcher.instance.onError = (e, st) {
    DiagLog.app.logError('приложение: необработанная ошибка', e, st);
    return true;
  };
  _lifecycle = AppLifecycleListener(onStateChange: (s) => DiagLog.app.log('[приложение] состояние: ${s.name}'));
  ControlHttpClient.onRouteChanged = (route, failed) =>
      DiagLog.app.log('связь с сервером (экран): $route${failed.isEmpty ? '' : ' (не вышло: $failed)'}');
  FlutterForegroundTask.initCommunicationPort();
  _initForegroundTask();
  runApp(const SvyazEstApp());
}

// Kept alive for the whole process.
// ignore: unused_element
AppLifecycleListener? _lifecycle;

void _initForegroundTask() {
  FlutterForegroundTask.init(
    androidNotificationOptions: AndroidNotificationOptions(
      channelId: 'svyazest_executor_service',
      channelName: 'Связь Есть? — работа',
      channelDescription: 'Показывает, работает ли приложение и ждёт ли оно заданий.',
      onlyAlertOnce: true,
    ),
    iosNotificationOptions: const IOSNotificationOptions(showNotification: false, playSound: false),
    foregroundTaskOptions: ForegroundTaskOptions(
      eventAction: ForegroundTaskEventAction.once(),
      autoRunOnBoot: true,
      autoRunOnMyPackageReplaced: true,
      allowWakeLock: true,
      allowWifiLock: false,
    ),
  );
}

class SvyazEstApp extends StatelessWidget {
  const SvyazEstApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Связь Есть?',
      debugShowCheckedModeBanner: false,
      theme: buildAppTheme(),
      navigatorObservers: [DiagNavObserver()],
      home: const StartupGate(),
    );
  }
}

/// What opens first: sign-in, the app, or «root» refusal. Also re-run after
/// signing in (login_screen.dart) and from the refusal's «Проверить снова».
class StartupGate extends StatelessWidget {
  const StartupGate({super.key});

  Future<_Startup> _load() async {
    final rooted = await NativeBridge.isRooted();
    final paired = await SecureStore.isPaired();
    if (!rooted) return _Startup(rooted: false, paired: paired);
    // Root (1.18.0): the admin can make an exception for a partner they
    // trust (admin panel → Партнёры → «Исключение: root и проверка
    // подлинности»). Signing in is allowed so the server knows who it is;
    // then the server's answer decides (cached for a start without network).
    if (!paired) return const _Startup(rooted: false, paired: false);
    bool exempt;
    try {
      exempt = (await ApiClient().profile()).integrityExempt;
      await SecureStore.setRootExempt(exempt);
    } catch (_) {
      exempt = await SecureStore.getRootExempt();
    }
    return _Startup(rooted: !exempt, paired: true);
  }

  @override
  Widget build(BuildContext context) {
    return FutureBuilder<_Startup>(
      future: _load(),
      builder: (context, snapshot) {
        if (snapshot.connectionState != ConnectionState.done) {
          return const Scaffold(
            backgroundColor: AppColors.bg,
            body: Center(child: CircularProgressIndicator(color: AppColors.accent)),
          );
        }
        final st = snapshot.data ?? const _Startup(rooted: false, paired: false);
        if (st.rooted) return const RootBlockedScreen();
        return st.paired ? const MainShell() : const LoginScreen();
      },
    );
  }
}

class _Startup {
  const _Startup({required this.rooted, required this.paired});
  final bool rooted;
  final bool paired;
}
