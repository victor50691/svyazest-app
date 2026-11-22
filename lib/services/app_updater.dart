import 'dart:io';
import 'package:path_provider/path_provider.dart';
import 'api_client.dart';
import 'diag_log.dart';
import 'native_bridge.dart';

/// A newer app version the server hands out (the same one the «Связь Есть?»
/// bot gives).
class AppUpdate {
  const AppUpdate({required this.version, required this.size, required this.sha256});
  final String version;
  final int size;
  final String sha256;
  String get sizeLabel => '${(size / (1024 * 1024)).toStringAsFixed(0)} МБ';
}

/// In-app update (1.19.0): ask the server for the latest version, download
/// the APK over the same routes as every request (Wi-Fi / VPN-client proxy /
/// mobile, ControlChannel), check it -- our package, signed with the same
/// certificate as this app, the SHA-256 the server announced -- and hand it
/// to the system installer. Android still asks the partner to tap
/// «Установить»; a silent install is not possible for an ordinary app.
class AppUpdater {
  static final _api = ApiClient();

  /// Numeric compare of «1.18.1» style versions (build suffix ignored).
  static bool isNewer(String a, String? b) {
    List<int> parts(String v) =>
        v.split('+').first.split('.').map((x) => int.tryParse(x.trim()) ?? 0).toList();
    if (b == null || b.isEmpty) return false;
    final x = parts(a), y = parts(b);
    for (var i = 0; i < 4; i++) {
      final p = i < x.length ? x[i] : 0, q = i < y.length ? y[i] : 0;
      if (p != q) return p > q;
    }
    return false;
  }

  /// The update, or null when this app is the latest (or the server has no file).
  static Future<AppUpdate?> check() async {
    await ApiClient.loadAppVersion();
    final j = await _api.appLatest();
    if (j == null || j['available'] != true) return null;
    final v = (j['version'] ?? '').toString();
    if (v.isEmpty || !isNewer(v, ApiClient.appVersion)) return null;
    return AppUpdate(version: v, size: (j['size'] as num?)?.toInt() ?? 0, sha256: (j['sha256'] ?? '').toString());
  }

  static Future<String> _path(AppUpdate u) async {
    final dir = Directory('${(await getTemporaryDirectory()).path}/updates');
    if (!dir.existsSync()) dir.createSync(recursive: true);
    // Older downloads are of no use any more.
    for (final f in dir.listSync()) {
      if (f is File && !f.path.endsWith('svyazest-${u.version}.apk')) {
        try { f.deleteSync(); } catch (_) {}
      }
    }
    return '${dir.path}/svyazest-${u.version}.apk';
  }

  static Future<bool> _verified(String path, AppUpdate u) async {
    if (!File(path).existsSync()) return false;
    final c = await NativeBridge.apkCheck(path);
    final ok = c['ok'] == true && (u.sha256.isEmpty || c['sha256'] == u.sha256) && c['version_name'] == u.version;
    DiagLog.app.log('[обновление] проверка ${u.version}: пакет=${c['package_ok']} подпись=${c['signer_ok']} '
        'версия=${c['version_name']} sha256 ${c['sha256'] == u.sha256 ? 'совпал' : 'НЕ совпал'}${c['error'] != null ? ' (${c['error']})' : ''}');
    return ok;
  }

  /// Download (or reuse an already downloaded, verified file). Throws with
  /// a message for the partner when something is wrong.
  static Future<String> download(AppUpdate u) async {
    final path = await _path(u);
    if (await _verified(path, u)) return path;
    final headers = await _api.authHeaders();
    if (headers == null) throw Exception('Нужно войти в приложение');
    DiagLog.app.log('[обновление] скачиваю ${u.version} (${u.sizeLabel})');
    final r = await NativeBridge.controlDownload('$apiBaseUrl/app/apk', headers, path);
    if (r['status'] != 200) {
      try { File(path).deleteSync(); } catch (_) {}
      throw Exception(r['status'] == 0 ? 'Нет связи с сервером' : 'Сервер ответил ${r['status']}');
    }
    DiagLog.app.log('[обновление] скачано ${r['saved']} Б через «${r['route']}»');
    if (!await _verified(path, u)) {
      try { File(path).deleteSync(); } catch (_) {}
      throw Exception('Файл обновления не прошёл проверку — попробуйте ещё раз');
    }
    return path;
  }

  /// Hand the file to the installer. false = the partner first has to allow
  /// installing from this app (the settings screen was opened for that).
  static Future<bool> install(String path) async {
    if (!await NativeBridge.canInstallApks()) {
      await NativeBridge.openInstallSettings();
      return false;
    }
    return NativeBridge.installApk(path);
  }
}
