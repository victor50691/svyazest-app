import 'package:flutter/material.dart';

import '../theme.dart';
import '../widgets/grouped_list.dart';

/// Which VPN client the user has. Happ and INCY come with screenshots
/// (assets/guides), anything else gets the generic text instruction.
enum VpnClient { happ, incy, other }

class _Step {
  const _Step(this.text, [this.image]);
  final String text;
  final String? image;
}

const _happSteps = [
  _Step('Откройте Happ и нажмите на шестерёнку (Настройки) в левом верхнем углу.', 'assets/guides/happ_1.jpg'),
  _Step('В разделе «Настройки Туннеля» откройте «Прокси для выбранных приложений».', 'assets/guides/happ_2.jpg'),
  _Step(
    'Выберите вкладку «Обход». В поиске введите «связь» и поставьте галочку напротив «Связь Есть?». '
    'Вернитесь назад и переподключите VPN.',
    'assets/guides/happ_3.jpg',
  ),
];

const _incySteps = [
  _Step('Откройте INCY и перейдите на вкладку «Настройки» в правом нижнем углу.', 'assets/guides/incy_1.jpg'),
  _Step('Откройте пункт «Прокси по приложениям».', 'assets/guides/incy_2.jpg'),
  _Step(
    'Включите переключатель «Включить прокси по приложениям» и выберите режим «Обход». '
    'В поиске введите «связь» и поставьте галочку напротив «Связь Есть?». Переподключите VPN, '
    'чтобы изменения применились.',
    'assets/guides/incy_3.jpg',
  ),
];

const _otherSteps = [
  _Step('Откройте настройки вашего VPN-клиента.'),
  _Step(
    'Найдите раздел с исключениями для приложений. Он может называться «Раздельное туннелирование», '
    '«Split tunneling», «Прокси для приложений», «Per-app proxy», «Обход для приложений» или '
    '«Приложения» в настройках маршрутизации.',
  ),
  _Step(
    'Выберите режим исключения — «Обход», «Bypass», «Исключить выбранные приложения». '
    'Режим «Только выбранные» не подходит: он, наоборот, пускает приложение через VPN.',
  ),
  _Step('В списке приложений найдите «Связь Есть?» (пакет com.svyazest.svyazest_app) и отметьте его.'),
  _Step('Отключите и снова включите VPN, чтобы изменения применились. Затем вернитесь в «Связь Есть?».'),
  _Step(
    'Если в вашем клиенте такой настройки нет, на время выполнения заданий выключайте VPN — '
    'проверки должны идти через мобильную сеть напрямую.',
  ),
];

extension VpnClientInfo on VpnClient {
  String get title => switch (this) {
        VpnClient.happ => 'Happ',
        VpnClient.incy => 'INCY',
        VpnClient.other => 'Другое',
      };

  String? get logo => switch (this) {
        VpnClient.happ => 'assets/guides/happ.png',
        VpnClient.incy => 'assets/guides/incy.png',
        VpnClient.other => null,
      };

  List<_Step> get _steps => switch (this) {
        VpnClient.happ => _happSteps,
        VpnClient.incy => _incySteps,
        VpnClient.other => _otherSteps,
      };
}

/// Rounded app logo (or a generic icon for «Другое») used as a row leading.
class VpnClientLogo extends StatelessWidget {
  const VpnClientLogo(this.client, {super.key, this.size = 28});
  final VpnClient client;
  final double size;

  @override
  Widget build(BuildContext context) {
    final logo = client.logo;
    return ClipRRect(
      borderRadius: BorderRadius.circular(size * 0.22),
      child: logo != null
          ? Image.asset(logo, width: size, height: size, fit: BoxFit.cover)
          : Container(
              width: size,
              height: size,
              color: AppColors.bg,
              child: Icon(Icons.vpn_key_outlined, size: size * 0.6, color: AppColors.textSecondary),
            ),
    );
  }
}

/// Opens the step-by-step instruction for one client.
void openVpnBypassGuide(BuildContext context, VpnClient client) {
  Navigator.of(context).push(MaterialPageRoute(builder: (_) => VpnBypassGuideScreen(client: client)));
}

/// The three client rows shown on the home screen under «Обход VPN:
/// Запрещён».
class VpnBypassClientsSection extends StatelessWidget {
  const VpnBypassClientsSection({super.key, this.header});
  final String? header;

  @override
  Widget build(BuildContext context) {
    return GroupedSection(
      header: header,
      children: [
        for (final c in VpnClient.values)
          GroupedRow(
            leading: VpnClientLogo(c),
            label: c.title,
            chevron: true,
            onTap: () => openVpnBypassGuide(context, c),
          ),
      ],
    );
  }
}

class VpnBypassGuideScreen extends StatelessWidget {
  const VpnBypassGuideScreen({super.key, required this.client});
  final VpnClient client;

  @override
  Widget build(BuildContext context) {
    final steps = client._steps;
    return Scaffold(
      appBar: AppBar(
        // «Другое» has no logo of its own -- just the title, no stand-in icon.
        title: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            if (client.logo != null) ...[VpnClientLogo(client, size: 24), const SizedBox(width: 10)],
            Text(client.title),
          ],
        ),
      ),
      body: ListView.separated(
        // targetSdk 36 draws edge-to-edge: the list runs under the system
        // navigation bar, and an explicit padding drops its inset -- without
        // it the end of the last step stayed hidden under the bar.
        padding: EdgeInsets.fromLTRB(16, 8, 16, 32 + MediaQuery.paddingOf(context).bottom),
        itemCount: steps.length + 1,
        separatorBuilder: (_, _) => const SizedBox(height: 20),
        itemBuilder: (context, i) {
          if (i == 0) {
            return Padding(
              padding: const EdgeInsets.symmetric(horizontal: 16),
              child: Text(
                client == VpnClient.other
                    ? 'Как исключить «Связь Есть?» из VPN-туннеля в любом клиенте:'
                    : 'Как исключить «Связь Есть?» из VPN-туннеля в ${client.title}:',
                style: const TextStyle(fontSize: 14, height: 1.45, color: AppColors.textSecondary),
              ),
            );
          }
          return _StepCard(index: i, step: steps[i - 1]);
        },
      ),
    );
  }
}

class _StepCard extends StatelessWidget {
  const _StepCard({required this.index, required this.step});
  final int index;
  final _Step step;

  @override
  Widget build(BuildContext context) {
    return Material(
      color: AppColors.group,
      borderRadius: BorderRadius.circular(12),
      clipBehavior: Clip.antiAlias,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 14, 16, 14),
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Container(
                  width: 26,
                  height: 26,
                  alignment: Alignment.center,
                  decoration: const BoxDecoration(color: AppColors.accent, shape: BoxShape.circle),
                  child: Text(
                    '$index',
                    style: const TextStyle(fontSize: 13, fontWeight: FontWeight.w700, color: Colors.white),
                  ),
                ),
                const SizedBox(width: 12),
                Expanded(
                  child: Padding(
                    padding: const EdgeInsets.only(top: 3),
                    child: Text(step.text, style: const TextStyle(fontSize: 15, height: 1.4)),
                  ),
                ),
              ],
            ),
          ),
          if (step.image != null)
            Padding(
              padding: const EdgeInsets.fromLTRB(16, 0, 16, 16),
              child: ClipRRect(
                borderRadius: BorderRadius.circular(10),
                child: Image.asset(step.image!, fit: BoxFit.fitWidth, width: double.infinity),
              ),
            ),
        ],
      ),
    );
  }
}

/// «Работа с включённым VPN» (1.11.0): why the app must go around the VPN,
/// the one step for the partner, and how the link to the server works under
/// «белые списки» (services/control_http.dart, ControlChannel.kt).
class VpnWorkGuideScreen extends StatelessWidget {
  const VpnWorkGuideScreen({super.key});

  static const _p = TextStyle(fontSize: 15, height: 1.45);
  static const _h = TextStyle(fontSize: 13, fontWeight: FontWeight.w600, color: AppColors.textSecondary, letterSpacing: .3);

  Widget _card(String text) => Material(
        color: AppColors.group,
        borderRadius: BorderRadius.circular(12),
        child: Padding(padding: const EdgeInsets.fromLTRB(16, 14, 16, 14), child: Text(text, style: _p)),
      );

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Работа с включённым VPN')),
      body: ListView(
        padding: EdgeInsets.fromLTRB(16, 8, 16, 32 + MediaQuery.paddingOf(context).bottom),
        children: [
          _card('VPN выключать не нужно — пользуйтесь интернетом как обычно. '
              'Нужно только, чтобы сами проверки «Связь Есть?» шли через мобильную сеть напрямую, мимо VPN: '
              'иначе приложение проверяло бы ваш VPN, а не мобильного оператора. Все остальные приложения '
              'на телефоне продолжат работать через VPN.'),
          const SizedBox(height: 20),
          const Padding(padding: EdgeInsets.only(left: 16, bottom: 8), child: Text('ШАГ 1. ДОБАВЬТЕ ПРИЛОЖЕНИЕ В ИСКЛЮЧЕНИЯ VPN', style: _h)),
          _card('В настройках вашего VPN-клиента отметьте «Связь Есть?» в списке приложений, которые идут мимо VPN '
              '(«Обход», «Исключения», «Раздельное туннелирование», «Per-app proxy»). Пошаговые инструкции:'),
          const SizedBox(height: 8),
          const VpnBypassClientsSection(),
          const SizedBox(height: 20),
          const Padding(padding: EdgeInsets.only(left: 16, bottom: 8), child: Text('ШАГ 2. ОБЫЧНО НИЧЕГО ДЕЛАТЬ НЕ НУЖНО', style: _h)),
          _card('Со своим сервером приложение связывается само и выбирает путь, который работает: через Wi-Fi, '
              'через VPN или через прокси вашего VPN-клиента. Какой путь используется сейчас, видно в '
              '«Настройки → Связь с сервером».\n\n'
              'Когда на мобильной сети включены белые списки и Wi-Fi нет, до сервера можно достучаться только через '
              'прокси VPN-клиента. В большинстве клиентов он уже работает сам (v2rayNG, Hiddify, NekoBox и другие) — '
              'приложение найдёт его автоматически.\n\n'
              'Если в «Связь с сервером» написано «нет связи»: откройте настройки VPN-клиента, найдите «Локальный прокси», '
              '«SOCKS5», «Порт SOCKS» или «Mixed port», включите его и посмотрите номер порта. Если порт необычный, '
              'впишите его в «Настройки → Связь с сервером → Порт прокси».'),
        ],
      ),
    );
  }
}
