import 'dart:convert';
import 'package:http/http.dart' as http;
import 'package:package_info_plus/package_info_plus.dart';

import 'control_http.dart';

import '../models/job.dart';
import '../models/profile.dart';
import 'native_bridge.dart';
import 'secure_store.dart';

/// Base URL for the executor API (admin-panel's routes/executor/, see
/// nginx's /executor/ location block). Overridable at build time with
/// `--dart-define=API_BASE_URL=...` -- not a secret, just a config default,
/// so it's fine to bake a working default into the open-source repo.
const String _defaultBaseUrl = 'https://dpichecker.st/executor/v1';
const String apiBaseUrl = String.fromEnvironment('API_BASE_URL', defaultValue: _defaultBaseUrl);

/// What the server sends back on every heartbeat: the «белые списки» probe
/// configuration, so the phone can measure the mode on its own schedule
/// instead of only while running a job.
class HeartbeatReply {
  const HeartbeatReply({
    this.whitelistAllowed = const [],
    this.whitelistControl = const [],
    this.whitelistInterval = const Duration(minutes: 5),
    this.whitelistRequired = true,
    this.diag,
    this.gps,
    this.attestDue = false,
  });

  /// The hardware-bound key is due for its daily re-attestation (1.16.0).
  final bool attestDue;

  /// Hosts that stay reachable under a whitelist, and neutral ones that do
  /// not -- the verdict is the comparison between them.
  final List<String> whitelistAllowed;
  final List<String> whitelistControl;
  final Duration whitelistInterval;

  /// Whether the server refuses jobs to phones outside whitelist mode
  /// (admin-configurable). Only affects what the app tells the user.
  final bool whitelistRequired;

  /// Support's log request, if any (services/diag_log.dart).
  final DiagInfo? diag;

  /// Support's GPS check request, if any (services/gps_check.dart).
  final GpsInfo? gps;

  bool get canProbe => whitelistAllowed.isNotEmpty && whitelistControl.isNotEmpty;
}

class ApiException implements Exception {
  ApiException(this.statusCode, this.message, [this.code]);
  final int statusCode;
  final String message;
  /// Machine-readable reason from the server ('hwid_banned',
  /// 'account_banned'), when it sends one.
  final String? code;
  bool get isDeviceBanned => code == 'hwid_banned';
  bool get isCountryBlocked => code == 'country_blocked';
  bool get isAccountBanned => code == 'account_banned';
  /// Below the minimum app version set in the admin panel: no jobs until
  /// the partner installs an update.
  bool get isAppOutdated => code == 'app_outdated';
  /// Token no longer valid: signed in elsewhere (one device per account),
  /// signed out from the panel, or the device row was retired.
  bool get isUnauthorized => statusCode == 401;
  @override
  String toString() => 'ApiException($statusCode, $message)';
}

class PairResult {
  PairResult(this.deviceToken, this.deviceId, this.executorId);
  final String deviceToken;
  final int deviceId;
  final String executorId;
}

class AuthStart {
  AuthStart(this.code, this.url);
  final String code;
  final String url;
}

/// status: 'pending' | 'ok' | 'expired'. `pair` is set only for 'ok'.
class AuthPoll {
  AuthPoll(this.status, [this.pair]);
  final String status;
  final PairResult? pair;
}

class ApiClient {
  // 1.11.0: requests to our server take their own route (VPN / Wi-Fi /
  // the VPN client's proxy / cellular), see control_http.dart; the checks
  // stay on the bare cellular network.
  ApiClient({http.Client? client}) : _client = client ?? ControlHttpClient();
  final http.Client _client;

  Uri _u(String path) => Uri.parse('$apiBaseUrl$path');

  /// This build's version (pubspec), sent as X-App-Version on every request
  /// so the server can enforce the minimum supported version. Loaded once
  /// per isolate: main() for the UI, onStart for the foreground service.
  static String? appVersion;

  static Future<void> loadAppVersion() async {
    if (appVersion != null) return;
    try {
      appVersion = (await PackageInfo.fromPlatform()).version;
    } catch (_) {}
  }

  Map<String, String> _jsonHeaders([String? token]) => {
        'Content-Type': 'application/json',
        if (token != null) 'Authorization': 'Bearer $token',
        'X-App-Version': ?appVersion,
      };

  Map<String, dynamic> _decode(http.Response resp) {
    Map<String, dynamic> body;
    try {
      body = jsonDecode(resp.body) as Map<String, dynamic>;
    } catch (_) {
      body = {};
    }
    if (resp.statusCode >= 400) {
      throw ApiException(resp.statusCode, (body['error'] as String?) ?? 'HTTP ${resp.statusCode}', body['code'] as String?);
    }
    return body;
  }

  Future<PairResult> pair({
    required String code,
    required String deviceModel,
    required String androidVersion,
    required String appVersion,
    String? hwid,
  }) async {
    final resp = await _client
        .post(
          _u('/pair'),
          headers: _jsonHeaders(),
          body: jsonEncode({
            'code': code,
            'device_model': deviceModel,
            'android_version': androidVersion,
            'app_version': appVersion,
            'hwid': ?hwid,
          }),
        )
        .timeout(const Duration(seconds: 15));
    final body = _decode(resp);
    return PairResult(body['device_token'] as String, body['device_id'] as int, body['executor_id'].toString());
  }

  /// Telegram sign-in, step 1: the server mints a one-time code and the deep
  /// link (`t.me/<bot>?start=app_<code>`) the user opens; step 2 polls it.
  Future<AuthStart> authStart({
    required String deviceModel,
    required String androidVersion,
    required String appVersion,
    String? hwid,
  }) async {
    final resp = await _client
        .post(
          _u('/auth/start'),
          headers: _jsonHeaders(),
          body: jsonEncode({
            'device_model': deviceModel,
            'android_version': androidVersion,
            'app_version': appVersion,
            'hwid': ?hwid,
          }),
        )
        .timeout(const Duration(seconds: 15));
    final body = _decode(resp);
    return AuthStart(body['code'] as String, body['url'] as String);
  }

  Future<AuthPoll> authPoll(String code) async {
    final resp = await _client
        .get(_u('/auth/poll?code=${Uri.encodeQueryComponent(code)}'), headers: _jsonHeaders())
        .timeout(const Duration(seconds: 15));
    if (resp.statusCode == 404) return AuthPoll('expired');
    final body = _decode(resp); // 403 (banned) surfaces as ApiException
    final status = (body['status'] as String?) ?? 'pending';
    if (status != 'ok') return AuthPoll(status);
    return AuthPoll(
      'ok',
      PairResult(body['device_token'] as String, body['device_id'] as int, body['executor_id'].toString()),
    );
  }

  /// Reports this phone's state and returns what the server wants back:
  /// the «белые списки» probe hosts and how often to re-run them. The
  /// verdict rides along on the next heartbeat after each measurement --
  /// the server only hands out jobs while it says the operator is in
  /// whitelist mode, so this is what keeps a phone in the pool.
  Future<HeartbeatReply> heartbeat({
    required bool mobileDataEnabled,
    required bool isOnline,
    String? operatorName,
    String? hwid,
    String? country,
    String? simCountry,
    String? whitelistVerdict,
    bool? ipv6,
    bool? rooted,
  }) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return const HeartbeatReply();
    final resp = await _client
        .post(
          _u('/heartbeat'),
          headers: _jsonHeaders(token),
          body: jsonEncode({
            'mobile_data_enabled': mobileDataEnabled,
            'is_online': isOnline,
            'operator_name': ?operatorName,
            'hwid': ?hwid,
            'country': ?country,
            'sim_country': ?simCountry,
            'whitelist_verdict': ?whitelistVerdict,
            // 1.17.2: whether the mobile network carries IPv6 right now
            // (measured every few minutes) -- only such phones are given
            // IPv6 checks of addresses / domains.
            'ipv6': ?ipv6,
            // 1.18.0: root found on the device. The server gives such a
            // phone jobs only if the admin made an exception for the
            // partner (executors.integrity_exempt).
            'rooted': ?rooted,
          }),
        )
        .timeout(const Duration(seconds: 15));
    final body = _decode(resp);
    return HeartbeatReply(
      whitelistAllowed: (body['whitelist_allowed'] as List?)?.map((e) => e.toString()).toList() ?? const [],
      whitelistControl: (body['whitelist_control'] as List?)?.map((e) => e.toString()).toList() ?? const [],
      whitelistInterval: Duration(seconds: (body['whitelist_interval'] as num?)?.toInt() ?? 300),
      whitelistRequired: body['whitelist_required'] != false,
      diag: DiagInfo.fromJson(body['diag']),
      gps: GpsInfo.fromJson(body['gps']),
      attestDue: body['attest_due'] == true,
    );
  }

  /// Long-polls for one job. The server holds the connection ~25s; the
  /// client timeout is set generously above that so a slow mobile link
  /// doesn't get misread as "no job available".
  Future<ExecutorJob?> pollJob() async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client.get(_u('/jobs/poll'), headers: _jsonHeaders(token)).timeout(
          const Duration(seconds: 40),
        );
    final body = _decode(resp);
    final job = body['job'];
    if (job == null) return null;
    return ExecutorJob.fromJson(job as Map<String, dynamic>);
  }

  /// Still checking [jobId]: the server extends its lease by 15 s. Without
  /// these pings a job whose phone vanished goes back to the queue quickly
  /// (admin-panel/routes/executor/index.js JOB_LEASE_SECONDS).
  Future<void> jobAlive(int jobId) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return;
    await _client
        .post(_u('/jobs/$jobId/alive'), headers: _jsonHeaders(token), body: '{}')
        .timeout(const Duration(seconds: 4));
  }

  Future<void> releaseJob(int jobId) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return;
    await _client
        .post(_u('/jobs/$jobId/release'), headers: _jsonHeaders(token), body: '{}')
        .timeout(const Duration(seconds: 10));
  }

  Future<void> submitResult(int jobId, {required bool success, required Map<String, dynamic> result}) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client
        .post(
          _u('/jobs/$jobId/result'),
          headers: _jsonHeaders(token),
          body: jsonEncode({'status': success ? 'completed' : 'failed', 'result': result}),
        )
        .timeout(const Duration(seconds: 15));
    _decode(resp);
  }

  /// The partner's answer to support's log request; returns its status now.
  Future<String> diagDecision(int id, bool accept) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client
        .post(_u('/diag/$id/decision'), headers: _jsonHeaders(token), body: jsonEncode({'accept': accept}))
        .timeout(const Duration(seconds: 15));
    return (_decode(resp)['status'] ?? '').toString();
  }

  /// The partner's answer to the GPS check request; returns its status now.
  Future<String> gpsDecision(int id, bool accept) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client
        .post(_u('/gps/$id/decision'), headers: _jsonHeaders(token), body: jsonEncode({'accept': accept}))
        .timeout(const Duration(seconds: 15));
    return (_decode(resp)['status'] ?? '').toString();
  }

  /// The one fix (or why there is none) for the GPS check.
  Future<void> gpsResult(int id, Map<String, dynamic> data) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client
        .post(_u('/gps/$id/result'), headers: _jsonHeaders(token), body: jsonEncode(data))
        .timeout(const Duration(seconds: 20));
    _decode(resp);
  }

  /// Настройки → «Остановить сбор логов».
  Future<void> diagStop(int id) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client
        .post(_u('/diag/$id/stop'), headers: _jsonHeaders(token), body: '{}')
        .timeout(const Duration(seconds: 15));
    _decode(resp);
  }

  /// One batch of the diagnostics log; false = the request is no longer
  /// accepted (stopped by support or the partner) -- stop collecting.
  Future<bool> diagLogs(int id, List<Map<String, dynamic>> lines) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return false;
    final resp = await _client
        .post(_u('/diag/$id/logs'), headers: _jsonHeaders(token), body: jsonEncode({'lines': lines}))
        .timeout(const Duration(seconds: 20));
    return _decode(resp)['active'] != false;
  }

  /// Diagnostics: last background-loop error, stored on the device row.
  Future<void> reportError(String message) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return;
    await _client
        .post(_u('/log'), headers: _jsonHeaders(token), body: jsonEncode({'message': message}))
        .timeout(const Duration(seconds: 10));
  }

  Future<String> attestChallenge() async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client
        .post(_u('/attest/challenge'), headers: _jsonHeaders(token), body: '{}')
        .timeout(const Duration(seconds: 15));
    return _decode(resp)['challenge'] as String;
  }

  /// Returns the server's verdict: verified | limited | unsupported | failed.
  Future<String> attestVerify({List<String>? chain, String? clientError}) async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client
        .post(
          _u('/attest/verify'),
          headers: _jsonHeaders(token),
          // 1.16.0: keep the attested key and bind it (job requests are signed with it).
          body: jsonEncode({'chain': ?chain, 'client_error': ?clientError, 'bind': chain != null}),
        )
        .timeout(const Duration(seconds: 30));
    return (_decode(resp)['status'] as String?) ?? 'error';
  }

  /// Tells the server which public IP the mobile operator gave this phone
  /// (the server reads it off the request) plus what the phone knows about
  /// its network, for the moderator to compare with the region/operator from
  /// the partner application. Sent over the cellular network explicitly; if
  /// that is impossible it goes the normal way marked via=default (then the
  /// IP is Wi-Fi's or a VPN's, and the server knows it). Best effort.
  /// Returns true when the report went out over the cellular network.
  Future<bool> reportNetwork() async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return false;
    final cc = await NativeBridge.networkCountry();
    final payload = <String, dynamic>{
      'operator_name': await NativeBridge.operatorName(),
      'network_country': cc.network,
      'sim_country': cc.sim,
      'vpn_active': await NativeBridge.hasActiveVpn(),
    };
    // 1.17.1: both public addresses of the mobile network, each asked of
    // Yandex's single-stack echo over cellular -- the request to us below
    // shows only one of them (IPv6 when the operator gives one, while the
    // moderator wants the IPv4 first).
    final both = await Future.wait([cellularEcho(4), cellularEcho(6)]);
    if (both[0].ip != null) payload['ipv4'] = both[0].ip;
    if (both[1].ip != null) payload['ipv6'] = both[1].ip;
    final ipErr = [
      if (both[0].error != null) 'ipv4: ${both[0].error}',
      if (both[1].error != null) 'ipv6: ${both[1].error}',
    ];
    if (ipErr.isNotEmpty) payload['ip_error'] = ipErr.join('; ');
    final url = _u('/network-report').toString();
    final cell = await NativeBridge.cellularPost(url, jsonEncode({...payload, 'via': 'cellular'}), token: token);
    if (cell.status != 0) return cell.ok; // reached the server over cellular
    try {
      await _client
          .post(_u('/network-report'),
              headers: _jsonHeaders(token), body: jsonEncode({...payload, 'via': 'default', 'error': cell.error}))
          .timeout(const Duration(seconds: 15));
    } catch (_) {}
    return false;
  }

  /// The operator's IPv4 (family 4) or IPv6 (6) over cellular via
  /// ipv4-internet / ipv6-internet.yandex.net (they answer the real address
  /// only to browser-like requests, see cellular_ip.dart). No IPv6 from the
  /// operator = a connect error, reported as such.
  static Future<({String? ip, String? error})> cellularEcho(int family) async {
    final r = await NativeBridge.cellularGet('https://ipv$family-internet.yandex.net/api/v0/ip', headers: const {
      'User-Agent': 'Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36',
      'Origin': 'https://yandex.ru',
      'Referer': 'https://yandex.ru/internet/',
    });
    if (!r.ok) return (ip: null, error: r.status != 0 ? 'HTTP ${r.status}' : (r.error ?? 'нет ответа'));
    try {
      final v = jsonDecode(r.body.trim());
      if (v is String && v.isNotEmpty && v != '127.0.0.1' && (family == 4 ? !v.contains(':') : v.contains(':'))) {
        return (ip: v, error: null);
      }
    } catch (_) {}
    final b = r.body.trim();
    return (ip: null, error: 'ответ ${b.length > 60 ? b.substring(0, 60) : b}');
  }

  /// In-app update (1.19.0): the version the server hands out --
  /// {version, available, size, sha256}; null without a token.
  Future<Map<String, dynamic>?> appLatest() async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return null;
    final resp = await _client.get(_u('/app/latest'), headers: _jsonHeaders(token)).timeout(const Duration(seconds: 20));
    return _decode(resp);
  }

  /// Headers for the native APK download (same auth as every request).
  Future<Map<String, String>?> authHeaders() async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) return null;
    return _jsonHeaders(token);
  }

  Future<ExecutorProfile> profile() async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp = await _client.get(_u('/profile'), headers: _jsonHeaders(token)).timeout(const Duration(seconds: 15));
    return ExecutorProfile.fromJson(_decode(resp));
  }

  Future<List<JobHistoryEntry>> jobHistory() async {
    final token = await SecureStore.getDeviceToken();
    if (token == null) throw ApiException(401, 'Not paired');
    final resp =
        await _client.get(_u('/jobs/history'), headers: _jsonHeaders(token)).timeout(const Duration(seconds: 15));
    final body = _decode(resp);
    final jobs = (body['jobs'] as List<dynamic>? ?? []);
    return jobs.map((j) => JobHistoryEntry.fromJson(j as Map<String, dynamic>)).toList();
  }
}
