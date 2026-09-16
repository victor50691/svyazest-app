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
    this.appOutdated = false,
    this.minAppVersion,
    this.updateUrl,
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
  /// This app is older than the minimum version set in the admin panel.
  final bool appOutdated;
  final String? minAppVersion;
  /// Where to download the update (admin panel → Настройки), may be empty.
  final String? updateUrl;

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
        appOutdated: json['app_outdated'] == true,
        minAppVersion: json['min_app_version'] as String?,
        updateUrl: json['update_url'] as String?,
      );
}
