import 'package:flutter/services.dart';

class NetworkState {
  const NetworkState({required this.mobile, required this.vpn, this.mobileSetting});
  /// A cellular network with internet is actually up right now.
  final bool mobile;
  final bool vpn;
  /// The phone's "mobile data" switch (null = unknown on old Android). On
  /// Wi-Fi the switch can be on while the network itself is down.
  final bool? mobileSetting;

  factory NetworkState.fromMap(Object? raw) {
    final m = (raw as Map?) ?? const {};
    return NetworkState(
      mobile: m['mobile'] == true,
      vpn: m['vpn'] == true,
      mobileSetting: m['mobileSetting'] is bool ? m['mobileSetting'] as bool : null,
    );
  }
}

/// Thin wrapper around NativePlugin.kt (android/). The plugin is attached to
/// both the UI engine and the foreground-service engine, so every method
/// here is safe to call from either isolate.
class NativeBridge {
  static const _channel = MethodChannel('com.svyazest.svyazest_app/native');
  static const _events = EventChannel('com.svyazest.svyazest_app/network');

  static Future<String> nativeLibraryDir() async {
    final dir = await _channel.invokeMethod<String>('nativeLibraryDir');
    return dir ?? '';
  }

  static Future<bool> hasMobileData() async {
    final result = await _channel.invokeMethod<bool>('hasMobileData');
    return result ?? false;
  }

  static Future<bool> hasActiveVpn() async {
    final result = await _channel.invokeMethod<bool>('hasActiveVpn');
    return result ?? false;
  }

  static Future<NetworkState> networkState() async {
    final raw = await _channel.invokeMethod<Object>('networkState');
    return NetworkState.fromMap(raw);
  }

  /// Emits the current state immediately, then again on every network
  /// change (ConnectivityManager callback) -- no polling.
  static Stream<NetworkState> networkChanges() =>
      _events.receiveBroadcastStream().map(NetworkState.fromMap);

  /// Keep a cellular Network alive (even next to Wi-Fi) while the job loop
  /// runs. Returns whether one is currently available.
  static Future<bool> cellularRequest(bool enable) async {
    try {
      return await _channel.invokeMethod<bool>('cellularRequest', {'enable': enable}) ?? false;
    } catch (_) {
      return false;
    }
  }

  /// Route every socket this process opens (Dart's dart:io included) via
  /// the cellular network, bypassing a third-party VPN. False = no cellular
  /// network to bind to right now.
  static Future<bool> bindProcessToCellular(bool enable, {int waitMs = 0}) async {
    return (await bindCellular(enable, waitMs: waitMs)).ok;
  }

  /// Like [bindProcessToCellular] but says why it failed: no cellular
  /// network at all, or the system refused (a VPN in "block connections
  /// without VPN" mode does that).
  static Future<({bool ok, bool cellular, String? error})> bindCellular(bool enable, {int waitMs = 0}) async {
    try {
      final raw = await _channel.invokeMethod<Object>('bindProcessToCellular', {'enable': enable, 'waitMs': waitMs});
      if (raw is bool) return (ok: raw, cellular: raw, error: raw ? null : 'unknown');
      final m = (raw as Map?) ?? const {};
      return (ok: m['ok'] == true, cellular: m['cellular'] == true, error: m['error']?.toString());
    } catch (e) {
      return (ok: false, cellular: false, error: e.toString());
    }
  }

  /// VPN kill-switch / "block connections without VPN" detection.
  static Future<({bool lockdown, bool vpn, String detail})> vpnLockdown() async {
    try {
      final raw = await _channel.invokeMethod<Object>('vpnLockdown');
      final m = (raw as Map?) ?? const {};
      return (lockdown: m['lockdown'] == true, vpn: m['vpn'] == true, detail: (m['detail'] ?? '').toString());
    } catch (e) {
      return (lockdown: false, vpn: false, detail: e.toString());
    }
  }

  static Future<void> openVpnSettings() async {
    try {
      await _channel.invokeMethod<bool>('openVpnSettings');
    } catch (_) {}
  }

  /// ISO alpha-2 of the mobile network (from the operator's MCC) and of
  /// the SIM -- what the server's country allowlist is checked against.
  static Future<({String? network, String? sim})> networkCountry() async {
    try {
      final raw = await _channel.invokeMethod<Object>('networkCountry');
      final m = (raw as Map?) ?? const {};
      return (network: m['network']?.toString(), sim: m['sim']?.toString());
    } catch (_) {
      return (network: null, sim: null);
    }
  }

  /// HTTPS POST that leaves over the cellular network no matter what the
  /// default route is (Wi-Fi, a VPN). status 0 = it could not be sent that
  /// way at all (no cellular network, or a VPN that forbids bypass).
  static Future<({bool ok, int status, String? error})> cellularPost(String url, String body,
      {String? token, int waitMs = 5000}) async {
    try {
      final raw = await _channel.invokeMethod<Object>(
          'cellularPost', {'url': url, 'body': body, 'token': token, 'waitMs': waitMs});
      final m = (raw as Map?) ?? const {};
      return (ok: m['ok'] == true, status: (m['status'] as num?)?.toInt() ?? 0, error: m['error']?.toString());
    } catch (e) {
      return (ok: false, status: 0, error: e.toString());
    }
  }

  /// Is the mobile «белые списки» mode on right now? ICMP + TCP:443 to
  /// [allowed] (must answer) and [control] (must not) over the cellular
  /// network. Returns the raw map: verdict (active | inactive | partial |
  /// no_internet | no_cellular | unknown) plus per-host results.
  static Future<Map<String, dynamic>> whitelistProbe(List<String> allowed, List<String> control) async {
    try {
      final raw = await _channel.invokeMethod<Object>('whitelistProbe', {'allowed': allowed, 'control': control, 'timeoutMs': 3000});
      return Map<String, dynamic>.from(_plain(raw) as Map);
    } catch (e) {
      return {'verdict': 'unknown', 'error': e.toString()};
    }
  }

  /// ICMP + TCP:443 to the checked IP/domain itself over the cellular
  /// network: host, ip, icmp_ms, tcp_ms (null = no answer), error.
  static Future<Map<String, dynamic>> hostProbe(String host) async {
    try {
      final raw = await _channel.invokeMethod<Object>('hostProbe', {'host': host, 'timeoutMs': 5000});
      return Map<String, dynamic>.from(_plain(raw) as Map);
    } catch (e) {
      return {'host': host, 'icmp_ms': null, 'tcp_ms': null, 'error': e.toString()};
    }
  }

  /// Walks all 256 addresses of a /24 over the cellular link (ICMP + TCP
  /// 443/80). Used by the subnet flavour of a БС check, where the question
  /// is whether the operator drops the whole range or only some hosts.
  static Future<Map<String, dynamic>> subnetProbe(String cidr,
      {int timeoutMs = 1200, int concurrency = 32}) async {
    try {
      final raw = await _channel.invokeMethod<Object>('subnetProbe', {
        'cidr': cidr,
        'timeoutMs': timeoutMs,
        'concurrency': concurrency,
      });
      return Map<String, dynamic>.from(_plain(raw) as Map);
    } catch (e) {
      return {'cidr': cidr, 'error': e.toString()};
    }
  }

  /// Platform-channel maps come back as Map<Object?, Object?>; jsonEncode
  /// needs String keys all the way down.
  static Object? _plain(Object? v) {
    if (v is Map) return v.map((k, val) => MapEntry(k.toString(), _plain(val)));
    if (v is List) return v.map(_plain).toList();
    return v;
  }

  static Future<String?> operatorName() async {
    try {
      return await _channel.invokeMethod<String>('operatorName');
    } catch (_) {
      return null;
    }
  }

  /// Bytes this app has sent+received since boot (TrafficStats, per uid).
  static Future<int> uidTrafficBytes() async {
    try {
      final raw = await _channel.invokeMethod<Object>('uidTraffic');
      final m = (raw as Map?) ?? const {};
      final rx = (m['rx'] as num?)?.toInt() ?? 0;
      final tx = (m['tx'] as num?)?.toInt() ?? 0;
      if (rx < 0 || tx < 0) return 0; // UNSUPPORTED on some devices
      return rx + tx;
    } catch (_) {
      return 0;
    }
  }

  /// Starts the embedded Xray-core with a JSON config. Null = started;
  /// otherwise the error text from the core.
  static Future<String?> xrayStart(String configJson) async {
    return _channel.invokeMethod<String>('xrayStart', {'config': configJson});
  }

  /// Stops the core; returns how many of its sockets were bound to the
  /// cellular network vs. not (see NativePlugin.socketBinder).
  static Future<({int bound, int unbound, String? error})> xrayStop() async {
    try {
      final raw = await _channel.invokeMethod<Object>('xrayStop');
      final m = (raw as Map?) ?? const {};
      return (
        bound: (m['bound'] as num?)?.toInt() ?? 0,
        unbound: (m['unbound'] as num?)?.toInt() ?? 0,
        error: m['error']?.toString(),
      );
    } catch (_) {
      return (bound: 0, unbound: 0, error: null);
    }
  }

  static Future<String> xrayVersion() async {
    try {
      return await _channel.invokeMethod<String>('xrayVersion') ?? '';
    } catch (_) {
      return '';
    }
  }

  static Future<String> hwid() async {
    try {
      return await _channel.invokeMethod<String>('hwid') ?? '';
    } catch (_) {
      return '';
    }
  }

  /// Key Attestation chain for [challengeB64], or the reason the phone
  /// could not produce one.
  static Future<({List<String>? chain, String? error})> attest(String challengeB64) async {
    try {
      final raw = await _channel.invokeMethod<Object>('attest', {'challenge': challengeB64});
      final m = (raw as Map?) ?? const {};
      if (m['chain'] is List) {
        return (chain: (m['chain'] as List).map((e) => e.toString()).toList(), error: null);
      }
      return (chain: null, error: (m['error'] ?? 'unknown').toString());
    } catch (e) {
      return (chain: null, error: e.toString());
    }
  }

  /// Notification + battery-optimization state, safe from any isolate.
  static Future<({bool notifications, bool battery})> permissions() async {
    try {
      final raw = await _channel.invokeMethod<Object>('permissions');
      final m = (raw as Map?) ?? const {};
      return (notifications: m['notifications'] != false, battery: m['battery'] != false);
    } catch (_) {
      return (notifications: true, battery: true);
    }
  }

  static Future<void> openNotificationSettings() async {
    try {
      await _channel.invokeMethod<bool>('openNotificationSettings');
    } catch (_) {}
  }

  static Future<bool> isRooted() async {
    try {
      final result = await _channel.invokeMethod<bool>('isRooted');
      return result ?? false;
    } catch (_) {
      return false;
    }
  }
}
