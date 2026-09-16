import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:device_info_plus/device_info_plus.dart';
import 'package:package_info_plus/package_info_plus.dart';
import 'package:url_launcher/url_launcher.dart';

import '../services/api_client.dart';
import '../services/native_bridge.dart';
import '../services/secure_store.dart';
import '../theme.dart';
import 'main_shell.dart';

/// Sign-in. Primary path: one tap opens the Связь Есть? bot with a one-time
/// deep link; the bot stamps the Telegram account on it and the app, which
/// keeps polling, receives its device token. Fallback: the manual code the
/// bot's «Код для входа» button still issues.
class LoginScreen extends StatefulWidget {
  const LoginScreen({super.key, this.notice});

  /// Optional one-line explanation shown above the button (why the user
  /// landed here, e.g. signed out because another device took the account).
  final String? notice;

  @override
  State<LoginScreen> createState() => _LoginScreenState();
}

class _LoginScreenState extends State<LoginScreen> {
  final _api = ApiClient();
  final _controller = TextEditingController();
  bool _busy = false;
  bool _waiting = false; // deep link opened, polling for the claim
  bool _manual = false; // manual-code field shown
  String? _error;
  String? _loginCode;
  Timer? _pollTimer;

  @override
  void initState() {
    super.initState();
    _error = widget.notice;
  }

  @override
  void dispose() {
    _pollTimer?.cancel();
    _controller.dispose();
    super.dispose();
  }

  // ---- Telegram ----

  Future<void> _startTelegram() async {
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final start = await _api.authStart(
        deviceModel: await _deviceModel(),
        androidVersion: await _androidVersion(),
        appVersion: await _appVersion(),
        hwid: await NativeBridge.hwid(),
      );
      _loginCode = start.code;
      final opened = await launchUrl(Uri.parse(start.url), mode: LaunchMode.externalApplication);
      if (!opened) {
        setState(() => _error = 'Не удалось открыть Telegram. Установите Telegram или войдите по коду.');
        return;
      }
      setState(() => _waiting = true);
      _pollTimer?.cancel();
      _pollTimer = Timer.periodic(const Duration(seconds: 2), (_) => _poll());
    } on ApiException catch (e) {
      setState(() => _error = e.message);
    } catch (_) {
      setState(() => _error = 'Не удалось подключиться. Проверьте интернет и попробуйте ещё раз.');
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _poll() async {
    final code = _loginCode;
    if (code == null || !_waiting) return;
    try {
      final r = await _api.authPoll(code);
      if (!mounted) return;
      if (r.status == 'ok' && r.pair != null) {
        _pollTimer?.cancel();
        await SecureStore.saveDevice(r.pair!.deviceToken, r.pair!.executorId);
        if (!mounted) return;
        Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => const MainShell()));
      } else if (r.status == 'expired') {
        _pollTimer?.cancel();
        setState(() {
          _waiting = false;
          _error = 'Ссылка для входа истекла. Попробуйте ещё раз.';
        });
      }
    } on ApiException catch (e) {
      if (e.isDeviceBanned || e.isAccountBanned || e.statusCode == 403) {
        _pollTimer?.cancel();
        if (mounted) {
          setState(() {
            _waiting = false;
            _error = e.message;
          });
        }
      }
      // Other errors: transient, keep polling until the link expires.
    } catch (_) {
      // Transient network error -- keep polling until the link expires.
    }
  }

  void _cancelWaiting() {
    _pollTimer?.cancel();
    setState(() {
      _waiting = false;
      _loginCode = null;
    });
  }

  // ---- manual code ----

  Future<void> _submitCode() async {
    final code = _controller.text.trim().toUpperCase();
    if (code.isEmpty) return;
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final result = await _api.pair(
        code: code,
        deviceModel: await _deviceModel(),
        androidVersion: await _androidVersion(),
        appVersion: await _appVersion(),
        hwid: await NativeBridge.hwid(),
      );
      await SecureStore.saveDevice(result.deviceToken, result.executorId);
      if (!mounted) return;
      Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => const MainShell()));
    } on ApiException catch (e) {
      setState(() => _error = e.statusCode == 404 ? 'Код неверный или истёк. Получите новый в боте.' : e.message);
    } catch (_) {
      setState(() => _error = 'Не удалось подключиться. Проверьте интернет и попробуйте ещё раз.');
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<String> _deviceModel() async {
    try {
      final info = await DeviceInfoPlugin().androidInfo;
      return '${info.manufacturer} ${info.model}';
    } catch (_) {
      return 'unknown';
    }
  }

  Future<String> _androidVersion() async {
    try {
      return (await DeviceInfoPlugin().androidInfo).version.release;
    } catch (_) {
      return 'unknown';
    }
  }

  Future<String> _appVersion() async {
    try {
      return (await PackageInfo.fromPlatform()).version;
    } catch (_) {
      return '0.0.0';
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      // The keyboard overlays the screen instead of squeezing it, so the
      // footer note stays put while the code field is being typed into.
      resizeToAvoidBottomInset: false,
      body: SafeArea(
        child: Stack(
          children: [
            ListView(
              padding: const EdgeInsets.fromLTRB(24, 96, 24, 120),
              children: [
                const Text(
                  'Вход',
                  style: TextStyle(fontSize: 28, fontWeight: FontWeight.w600, letterSpacing: -0.3),
                ),
                const SizedBox(height: 8),
                Text(
                  _waiting
                      ? 'Подтвердите вход в боте «Связь Есть?» и вернитесь сюда.'
                      : 'Войдите через свой аккаунт Telegram: откроется бот «Связь Есть?», в нём нажмите «Start».',
                  style: const TextStyle(fontSize: 15, height: 1.4, color: AppColors.textSecondary),
                ),
                const SizedBox(height: 24),
                if (_waiting) ...[
                  const Row(
                    children: [
                      SizedBox(width: 18, height: 18, child: CircularProgressIndicator(strokeWidth: 2)),
                      SizedBox(width: 12),
                      Text('Ожидаем подтверждение в Telegram…', style: TextStyle(fontSize: 15)),
                    ],
                  ),
                  const SizedBox(height: 20),
                  TextButton(
                    onPressed: _cancelWaiting,
                    style: TextButton.styleFrom(foregroundColor: AppColors.textSecondary),
                    child: const Text('Отмена'),
                  ),
                ] else ...[
                  ElevatedButton(
                    onPressed: _busy ? null : _startTelegram,
                    child: _busy && !_manual
                        ? const SizedBox(
                            height: 20,
                            width: 20,
                            child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white),
                          )
                        : const Text('Войти через Telegram'),
                  ),
                  if (_error != null) ...[
                    const SizedBox(height: 10),
                    Padding(
                      padding: const EdgeInsets.symmetric(horizontal: 4),
                      child: Text(_error!, style: const TextStyle(color: AppColors.danger, fontSize: 13)),
                    ),
                  ],
                  const SizedBox(height: 12),
                  if (!_manual)
                    Center(
                      child: TextButton(
                        onPressed: () => setState(() => _manual = true),
                        style: TextButton.styleFrom(foregroundColor: AppColors.textSecondary),
                        child: const Text('Ввести код вручную'),
                      ),
                    )
                  else ...[
                    const SizedBox(height: 12),
                    const Text(
                      'Код выдаёт бот по кнопке «Код для входа», он действует 10 минут.',
                      style: TextStyle(fontSize: 13, height: 1.4, color: AppColors.textSecondary),
                    ),
                    const SizedBox(height: 12),
                    TextField(
                      controller: _controller,
                      textCapitalization: TextCapitalization.characters,
                      maxLength: 8,
                      style: const TextStyle(fontSize: 20, letterSpacing: 4, fontWeight: FontWeight.w500),
                      inputFormatters: [UpperCaseTextFormatter()],
                      decoration: const InputDecoration(counterText: '', hintText: 'Код'),
                      onSubmitted: (_) => _submitCode(),
                    ),
                    const SizedBox(height: 12),
                    OutlinedButton(
                      onPressed: _busy ? null : _submitCode,
                      style: OutlinedButton.styleFrom(
                        foregroundColor: AppColors.textPrimary,
                        side: const BorderSide(color: AppColors.separator),
                        minimumSize: const Size.fromHeight(50),
                        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(12)),
                      ),
                      child: const Text('Подключить по коду'),
                    ),
                  ],
                ],
              ],
            ),
            const Positioned(
              left: 24,
              right: 24,
              bottom: 24,
              child: Text(
                'Приложение выполняет проверки только через мобильный интернет. Задания можно остановить в любой момент.',
                style: TextStyle(fontSize: 13, height: 1.45, color: AppColors.textSecondary),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

class UpperCaseTextFormatter extends TextInputFormatter {
  @override
  TextEditingValue formatEditUpdate(TextEditingValue oldValue, TextEditingValue newValue) {
    return newValue.copyWith(text: newValue.text.toUpperCase());
  }
}
