class ExecutorProfile {
  ExecutorProfile({
    required this.id,
    required this.balance,
    required this.totalEarned,
    required this.totalJobsCompleted,
    this.attestationStatus,
    this.trafficBytesTotal = 0,
    this.country,
    this.countryAllowed = true,
    this.allowedCountries = const [],
    this.moderationStatus = 'approved',
    this.moderationReason,
    this.operatorLabel,
    this.region,
    this.federalDistrictLabel,
    this.pendingApplication,
    this.appOutdated = false,
    this.minAppVersion,
    this.updateUrl,
    this.diag,
    this.availableBalance,
    this.heldBalance = 0,
    this.holdDays = 0,
    this.level,
    this.gps,
    this.integrityExempt = false,
  });

  final String id;
  final double balance;
  final double totalEarned;
  final int totalJobsCompleted;
  /// verified | limited | unsupported | failed | null (not attested yet)
  final String? attestationStatus;
  /// Data the checks consumed on this account, summed over all jobs.
  final int trafficBytesTotal;
  final String? country;
  final bool countryAllowed;
  final List<String> allowedCountries;
  /// Partner application in the Telegram bot: pending | approved | rejected.
  /// Only 'approved' may switch the work toggle on.
  final String moderationStatus;
  final String? moderationReason;
  bool get isApproved => moderationStatus == 'approved';

  /// What the partner works as, as approved by moderation: the mobile
  /// operator («Билайн»), the region typed in the application and the
  /// federal district the admin assigned. Null until the first application
  /// is approved.
  final String? operatorLabel;
  final String? region;
  final String? federalDistrictLabel;

  /// The application still waiting for review, if any -- the initial one
  /// (nothing approved yet) or a change to the approved data, which keeps
  /// working on the old values until it is reviewed.
  final PartnerApplication? pendingApplication;

  /// Something worth showing: either approved data or an application.
  bool get hasPartnerInfo =>
      (operatorLabel ?? '').isNotEmpty || (region ?? '').isNotEmpty || pendingApplication != null;
  /// This app is older than the minimum version set in the admin panel.
  final bool appOutdated;
  final String? minAppVersion;
  /// Where to download the update (admin panel → Настройки), may be empty.
  final String? updateUrl;
  /// Support's request to collect this device's log (pending / accepted).
  final DiagInfo? diag;
  /// Hold on withdrawals: what can be withdrawn now (null = old server ->
  /// the whole balance), what is still held, and for how many days.
  final double? availableBalance;
  final double heldBalance;
  final int holdDays;
  /// Level, progress, streak, achievements (null = old server).
  final LevelInfo? level;
  /// Support's one-time GPS check request (services/gps_check.dart).
  final GpsInfo? gps;
  /// The admin let this partner work on a rooted device / without passing
  /// Key Attestation (executors.integrity_exempt, 1.18.0).
  final bool integrityExempt;

  factory ExecutorProfile.fromJson(Map<String, dynamic> json) => ExecutorProfile(
        id: json['id'].toString(),
        balance: double.tryParse(json['balance'].toString()) ?? 0,
        totalEarned: double.tryParse(json['total_earned'].toString()) ?? 0,
        totalJobsCompleted: (json['total_jobs_completed'] as num?)?.toInt() ?? 0,
        attestationStatus: json['attestation_status'] as String?,
        trafficBytesTotal: (json['traffic_bytes_total'] as num?)?.toInt() ?? 0,
        country: json['country'] as String?,
        countryAllowed: json['country_allowed'] != false,
        allowedCountries: (json['allowed_countries'] as List?)?.map((e) => e.toString()).toList() ?? const [],
        moderationStatus: (json['moderation_status'] as String?) ?? 'approved',
        moderationReason: json['moderation_reason'] as String?,
        operatorLabel: json['operator_label'] as String?,
        region: json['region'] as String?,
        federalDistrictLabel: json['federal_district_label'] as String?,
        pendingApplication: json['pending_application'] is Map<String, dynamic>
            ? PartnerApplication.fromJson(json['pending_application'] as Map<String, dynamic>)
            : null,
        appOutdated: json['app_outdated'] == true,
        integrityExempt: json['integrity_exempt'] == true,
        minAppVersion: json['min_app_version'] as String?,
        updateUrl: json['update_url'] as String?,
        diag: DiagInfo.fromJson(json['diag']),
        availableBalance: json['available_balance'] == null ? null : double.tryParse(json['available_balance'].toString()),
        heldBalance: double.tryParse((json['held_balance'] ?? 0).toString()) ?? 0,
        holdDays: (json['hold_days'] as num?)?.toInt() ?? 0,
        level: json['level'] is Map<String, dynamic> ? LevelInfo.fromJson(json['level'] as Map<String, dynamic>) : null,
        gps: GpsInfo.fromJson(json['gps']),
      );
}

/// A partner application waiting for moderation: what the person typed in
/// the «Связь Есть?» bot (operator, region) and the federal district the
/// admin picks when approving it.
class PartnerApplication {
  PartnerApplication({this.kind = 'initial', this.operatorLabel, this.region, this.federalDistrictLabel});

  /// initial = nothing approved yet; change = editing already approved data.
  final String kind;
  final String? operatorLabel;
  final String? region;
  final String? federalDistrictLabel;

  bool get isChange => kind == 'change';

  factory PartnerApplication.fromJson(Map<String, dynamic> json) => PartnerApplication(
        kind: (json['kind'] as String?) ?? 'initial',
        operatorLabel: json['operator_label'] as String?,
        region: json['region'] as String?,
        federalDistrictLabel: json['federal_district_label'] as String?,
      );
}

/// Support's request to collect this device's log for diagnostics (admin
/// panel → Партнёры → «Логи»): pending = waiting for the partner's answer,
/// accepted = collecting (services/diag_log.dart).
class DiagInfo {
  const DiagInfo(this.id, this.status, [this.reason = '']);
  final int id;
  final String status;
  /// Why support asks (required in the admin panel since 2026-11-21).
  final String reason;

  static DiagInfo? fromJson(Object? raw) {
    if (raw is Map && raw['id'] is num) {
      return DiagInfo((raw['id'] as num).toInt(), (raw['status'] ?? '').toString(), (raw['reason'] ?? '').toString());
    }
    return null;
  }
}

/// «Связь Есть?» level as the server counts it (admin-panel/services/
/// executorLevels.js profileInfo).
class LevelInfo {
  LevelInfo({
    required this.level,
    required this.points,
    required this.levelMin,
    this.nextMin,
    required this.share,
    this.nextShare,
    this.streakDays = 0,
    this.streakToday = false,
    this.achievements = const [],
  });

  final int level;
  final int points;
  final int levelMin;
  final int? nextMin;
  final double share;
  final double? nextShare;
  final int streakDays;
  final bool streakToday;
  final List<Achievement> achievements;

  factory LevelInfo.fromJson(Map<String, dynamic> j) => LevelInfo(
        level: (j['level'] as num?)?.toInt() ?? 1,
        points: (j['points'] as num?)?.toInt() ?? 0,
        levelMin: (j['level_min'] as num?)?.toInt() ?? 0,
        nextMin: (j['next_min'] as num?)?.toInt(),
        share: (j['share'] as num?)?.toDouble() ?? 0,
        nextShare: (j['next_share'] as num?)?.toDouble(),
        streakDays: (j['streak_days'] as num?)?.toInt() ?? 0,
        streakToday: j['streak_today'] == true,
        achievements: (j['achievements'] as List? ?? const [])
            .whereType<Map<String, dynamic>>()
            .map(Achievement.fromJson)
            .toList(),
      );
}

class Achievement {
  Achievement({required this.code, required this.emoji, required this.title, required this.desc, this.earnedAt});
  final String code;
  final String emoji;
  final String title;
  final String desc;
  final DateTime? earnedAt;

  factory Achievement.fromJson(Map<String, dynamic> j) => Achievement(
        code: (j['code'] ?? '').toString(),
        emoji: (j['emoji'] ?? '🏅').toString(),
        title: (j['title'] ?? '').toString(),
        desc: (j['desc'] ?? '').toString(),
        earnedAt: j['earned_at'] == null ? null : DateTime.tryParse(j['earned_at'].toString()),
      );
}

/// Support's GPS check request: pending (waiting for the partner) or
/// accepted (allowed, the fix not sent yet).
class GpsInfo {
  const GpsInfo(this.id, this.status, [this.reason = '']);
  final int id;
  final String status;
  /// Why support asks (required in the admin panel since 2026-11-21).
  final String reason;

  static GpsInfo? fromJson(Object? raw) {
    if (raw is Map && raw['id'] is num) {
      return GpsInfo((raw['id'] as num).toInt(), (raw['status'] ?? '').toString(), (raw['reason'] ?? '').toString());
    }
    return null;
  }
}
