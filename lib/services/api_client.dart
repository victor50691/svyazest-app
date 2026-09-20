import 'dart:convert';
import 'package:http/http.dart' as http;
import 'package:package_info_plus/package_info_plus.dart';

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
  });

  /// Hosts that stay reachable under a whitelist, and neutral ones that do
  /// not -- the verdict is the comparison between them.
  final List<String> whitelistAllowed;
  final List<String> whitelistControl;
  final Duration whitelistInterval;

  /// Whether the server refuses jobs to phones outside whitelist mode
  /// (admin-configurable). Only affects what the app tells the user.
  final bool whitelistRequired;

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
  ApiClient({http.Client? client}) : _client = client ?? http.Client();
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
        if (appVersion != null) 'X-App-Version': appVersion!,
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
          }),
        )
        .timeout(const Duration(seconds: 15));
    final body = _decode(resp);
    return HeartbeatReply(
      whitelistAllowed: (body['whitelist_allowed'] as List?)?.map((e) => e.toString()).toList() ?? const [],
      whitelistControl: (body['whitelist_control'] as List?)?.map((e) => e.toString()).toList() ?? const [],
      whitelistInterval: Duration(seconds: (body['whitelist_interval'] as num?)?.toInt() ?? 300),
      whitelistRequired: body['whitelist_required'] != false,
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
          body: jsonEncode({'chain': ?chain, 'client_error': ?clientError}),
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
