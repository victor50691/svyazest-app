import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';

import 'screens/main_shell.dart';
import 'screens/login_screen.dart';
import 'screens/root_blocked_screen.dart';
import 'services/api_client.dart';
import 'services/native_bridge.dart';
import 'services/secure_store.dart';
import 'theme.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await ApiClient.loadAppVersion();
  FlutterForegroundTask.initCommunicationPort();
  _initForegroundTask();
  runApp(const SvyazEstApp());
}

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
      home: const _StartupGate(),
    );
  }
}

class _StartupGate extends StatelessWidget {
  const _StartupGate();

  Future<_Startup> _load() async {
    final rooted = await NativeBridge.isRooted();
    final paired = rooted ? false : await SecureStore.isPaired();
    return _Startup(rooted: rooted, paired: paired);
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
