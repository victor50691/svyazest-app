import 'api_client.dart';
import 'native_bridge.dart';

/// Runs one Android Key Attestation round: nonce from the server, key made
/// in the secure hardware with that nonce, chain back to the server, verdict.
/// Cheap (one key generation), so it runs on every app launch.
class Attestation {
  static final _api = ApiClient();
  static Future<String>? _inFlight;

  static Future<String> run() {
    return _inFlight ??= _run().whenComplete(() => _inFlight = null);
  }

  static Future<String> _run() async {
    try {
      final challenge = await _api.attestChallenge();
      final r = await NativeBridge.attest(challenge);
      if (r.chain == null) {
        return await _api.attestVerify(clientError: r.error);
      }
      return await _api.attestVerify(chain: r.chain);
    } on ApiException {
      rethrow;
    } catch (_) {
      return 'error';
    }
  }

  static String label(String? status) {
    switch (status) {
      case 'verified':
        return 'Подтверждена';
      case 'limited':
        return 'Ограниченный режим';
      case 'unsupported':
        return 'Не поддерживается';
      case 'failed':
        return 'Не пройдена';
      default:
        return '—';
    }
  }
}
