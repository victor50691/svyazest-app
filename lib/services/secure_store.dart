import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';

/// The device_token is the app's only credential (see admin-panel's
/// middleware/executorAuth.js) -- worth the extra cost of the Keystore-backed
/// secure storage plugin over plain SharedPreferences.
class SecureStore {
  static const _storage = FlutterSecureStorage(
    aOptions: AndroidOptions(encryptedSharedPreferences: true),
  );

  static const _keyDeviceToken = 'device_token';
  static const _keyExecutorId = 'executor_id';

  static Future<void> saveDevice(String token, String executorId) async {
    await _storage.write(key: _keyDeviceToken, value: token);
    await _storage.write(key: _keyExecutorId, value: executorId);
    // Mirror for the foreground-service isolate: flutter_foreground_task's
    // own store is guaranteed readable there, the Keystore-backed plugin is
    // not on every device/engine combination.
    try {
      await FlutterForegroundTask.saveData(key: _keyDeviceToken, value: token);
    } catch (_) {}
  }

  static Future<String?> getDeviceToken() async {
    try {
      final v = await _storage.read(key: _keyDeviceToken);
      if (v != null) return v;
    } catch (_) {
      // fall through to the mirror
    }
    try {
      return await FlutterForegroundTask.getData<String>(key: _keyDeviceToken);
    } catch (_) {
      return null;
    }
  }

  static Future<String?> getExecutorId() => _storage.read(key: _keyExecutorId);

  static Future<bool> isPaired() async => (await getDeviceToken()) != null;

  static Future<void> clear() async {
    await _storage.delete(key: _keyDeviceToken);
    await _storage.delete(key: _keyExecutorId);
    try {
      await FlutterForegroundTask.removeData(key: _keyDeviceToken);
    } catch (_) {}
  }
}
