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

  /// The server's last answer to «may this rooted device work» (1.18.0),
  /// for a start without network.
  /// Kept in the foreground task's store too: the service isolate reads it
  /// there when secure storage is not available to it (like the token).
  static const _keyRootExempt = 'root_exempt';
  static Future<void> setRootExempt(bool v) async {
    try {
      await _storage.write(key: _keyRootExempt, value: v ? '1' : '0');
    } catch (_) {}
    try {
      await FlutterForegroundTask.saveData(key: _keyRootExempt, value: v);
    } catch (_) {}
  }

  static Future<bool> getRootExempt() async {
    try {
      final v = await _storage.read(key: _keyRootExempt);
      if (v != null) return v == '1';
    } catch (_) {}
    try {
      return await FlutterForegroundTask.getData<bool>(key: _keyRootExempt) ?? false;
    } catch (_) {
      return false;
    }
  }

  static Future<void> clear() async {
    await _storage.delete(key: _keyDeviceToken);
    await _storage.delete(key: _keyExecutorId);
    try {
      await FlutterForegroundTask.removeData(key: _keyDeviceToken);
    } catch (_) {}
  }
}
