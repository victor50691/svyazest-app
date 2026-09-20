import '../native_bridge.dart';

/// A whole /24 as this phone's mobile network sees it.
///
/// A single-address check can only say "this host does not answer here",
/// which reads the same whether the operator filters that one address or
/// blackholes the entire range. Walking all 256 tells those apart: if
/// nothing in the subnet answers, the range is dark for this operator; if
/// some hosts answer and others don't, the filtering is per-address.
///
/// Nothing here decides "blocked" on its own -- an empty subnet also has no
/// live hosts. The verdict carries the counts, and the report says what was
/// measured rather than inferring intent.
class SubnetCheckResult {
  SubnetCheckResult({
    required this.cidr,
    this.probed = 0,
    this.total = 256,
    this.alive = 0,
    this.aliveIcmp = 0,
    this.aliveTcp = 0,
    this.hosts = const [],
    this.elapsedMs,
    this.error,
    this.untestable = false,
    this.untestableReason,
  });

  final String cidr;
  final int probed;
  final int total;
  final int alive;
  final int aliveIcmp;
  final int aliveTcp;
  final List<Map<String, dynamic>> hosts;
  final int? elapsedMs;
  final String? error;
  final bool untestable;
  final String? untestableReason;

  /// 'alive'   -- something in the range answers, so it is not dark;
  /// 'silent'  -- all 256 stayed quiet on ICMP and TCP alike;
  /// 'partial' -- answers came only over TCP or only over ICMP, which is
  ///              what per-protocol filtering looks like from here.
  String get verdict {
    if (untestable || error != null) return 'unknown';
    if (alive == 0) return 'silent';
    if (aliveIcmp == 0 || aliveTcp == 0) return 'partial';
    return 'alive';
  }

  Map<String, dynamic> toJson() => {
        'cidr': cidr,
        'probed': probed,
        'total': total,
        'alive': alive,
        'alive_icmp': aliveIcmp,
        'alive_tcp': aliveTcp,
        'hosts': hosts,
        'elapsed_ms': elapsedMs,
        'verdict': verdict,
        'error': error,
        'untestable': untestable,
        'untestable_reason': untestableReason,
        // The bot's report reads the same keys for every resource type.
        'works': alive > 0,
        'connected': alive > 0,
      };
}

Future<SubnetCheckResult> checkSubnet(String cidr) async {
  final raw = await NativeBridge.subnetProbe(cidr);
  final error = raw['error'] as String?;
  if (error != null) {
    // no_cellular means the phone could not leave the mobile network at all:
    // the same untestable code the VPN checker reports, so the server side
    // refunds instead of recording a verdict nobody measured.
    final untestable = error == 'no_cellular' || error == 'bad_cidr';
    return SubnetCheckResult(
      cidr: cidr,
      error: error,
      untestable: untestable,
      untestableReason: error == 'no_cellular' ? 'cellular_unavailable' : error,
    );
  }
  return SubnetCheckResult(
    cidr: cidr,
    probed: (raw['probed'] as num?)?.toInt() ?? 0,
    total: (raw['total'] as num?)?.toInt() ?? 256,
    alive: (raw['alive'] as num?)?.toInt() ?? 0,
    aliveIcmp: (raw['alive_icmp'] as num?)?.toInt() ?? 0,
    aliveTcp: (raw['alive_tcp'] as num?)?.toInt() ?? 0,
    hosts: (raw['hosts'] as List?)
            ?.map((e) => Map<String, dynamic>.from(e as Map))
            .toList() ??
        const [],
    elapsedMs: (raw['elapsed_ms'] as num?)?.toInt(),
  );
}
