import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';

import '../screens/blocked_screen.dart';
import '../screens/login_screen.dart';
import 'api_client.dart';
import 'secure_store.dart';

/// One place that turns an auth failure from the API into what the user
/// sees: 401 -> signed out (another device took this account, or the panel
/// retired it); 403 hwid_banned / account_banned -> blocked screen.
/// Returns true when it handled the error (the caller should stop).
Future<bool> handleAuthFailure(BuildContext context, Object error) async {
  if (error is! ApiException) return false;
  if (!(error.isUnauthorized || error.isDeviceBanned || error.isAccountBanned)) return false;
  await FlutterForegroundTask.stopService();
  await SecureStore.clear();
  if (!context.mounted) return true;
  final Widget next;
  if (error.isDeviceBanned) {
    next = const BlockedScreen(
      title: 'Устройство заблокировано',
      body: 'Этот телефон заблокирован администратором «Связь Есть?». Переустановка приложения или другой аккаунт Telegram не помогут.',
    );
  } else if (error.isAccountBanned) {
    next = const BlockedScreen(
      title: 'Доступ заблокирован',
      body: 'Ваш аккаунт заблокирован. Подробности — в боте «Связь Есть?».',
    );
  } else {
    next = const LoginScreen(
      notice: 'Сеанс завершён: в аккаунт вошли с другого телефона или устройство отключено. Войдите снова.',
    );
  }
  Navigator.of(context).pushAndRemoveUntil(MaterialPageRoute(builder: (_) => next), (route) => false);
  return true;
}
