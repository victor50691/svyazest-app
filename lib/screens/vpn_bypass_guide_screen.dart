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
        title: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            VpnClientLogo(client, size: 24),
            const SizedBox(width: 10),
            Text(client.title),
          ],
        ),
      ),
      body: ListView.separated(
        padding: const EdgeInsets.fromLTRB(16, 8, 16, 32),
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
