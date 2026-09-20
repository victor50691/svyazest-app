class ExecutorJob {
  ExecutorJob({
    required this.id,
    required this.checkId,
    required this.resource,
    required this.resourceType,
    this.protocol,
    this.probeSites = const [],
    this.downloadUrl,
    this.whitelistAllowed = const [],
    this.whitelistControl = const [],
  });

  final int id;
  final int checkId;
  final String resource;
  final String resourceType; // 'vpn_key' | 'ip' | 'domain' | 'subnet'
  final String? protocol; // vless/vmess/trojan/shadowsocks/hysteria2
  /// Sites to open through the key and a ~1 MB file to pull through it
  /// (server-configured; see routes/executor/index.js probeConfig).
  final List<String> probeSites;
  final String? downloadUrl;
  /// «Белые списки» probe (server-configured): hosts that must answer under
  /// the mobile whitelist mode, and neutral hosts that must not.
  final List<String> whitelistAllowed;
  final List<String> whitelistControl;

  factory ExecutorJob.fromJson(Map<String, dynamic> json) => ExecutorJob(
        id: json['id'] as int,
        checkId: json['check_id'] as int,
        resource: json['resource'] as String,
        resourceType: json['resource_type'] as String,
        protocol: json['protocol'] as String?,
        probeSites: (json['probe_sites'] as List?)?.map((e) => e.toString()).toList() ?? const [],
        downloadUrl: json['download_url'] as String?,
        whitelistAllowed: (json['whitelist_allowed'] as List?)?.map((e) => e.toString()).toList() ?? const [],
        whitelistControl: (json['whitelist_control'] as List?)?.map((e) => e.toString()).toList() ?? const [],
      );
}

class JobHistoryEntry {
  JobHistoryEntry({
    required this.id,
    required this.resourceType,
    required this.status,
    required this.rewardAmount,
    required this.createdAt,
    this.protocol,
    this.completedAt,
  });

  final int id;
  final String resourceType;
  final String? protocol;
  final String status;
  final double rewardAmount;
  final DateTime createdAt;
  final DateTime? completedAt;

  factory JobHistoryEntry.fromJson(Map<String, dynamic> json) => JobHistoryEntry(
        id: json['id'] as int,
        resourceType: json['resource_type'] as String,
        protocol: json['protocol'] as String?,
        status: json['status'] as String,
        rewardAmount: double.tryParse(json['reward_amount'].toString()) ?? 0,
        createdAt: DateTime.parse(json['created_at'] as String),
        completedAt: json['completed_at'] != null ? DateTime.parse(json['completed_at'] as String) : null,
      );
}
