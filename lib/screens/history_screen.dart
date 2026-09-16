import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';

import '../models/job.dart';
import '../services/api_client.dart';
import '../theme.dart';
import '../widgets/grouped_list.dart';

class HistoryTab extends StatefulWidget {
  const HistoryTab({super.key});

  @override
  State<HistoryTab> createState() => _HistoryTabState();
}

class _HistoryTabState extends State<HistoryTab> with AutomaticKeepAliveClientMixin, WidgetsBindingObserver {
  final _api = ApiClient();
  List<JobHistoryEntry>? _jobs; // null = not loaded yet
  bool _error = false;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    // Live list: a job finishing in the service refreshes it at once, a
    // timer keeps it current in between, and coming back to the app
    // refreshes too -- no pull-to-refresh needed (still works though).
    FlutterForegroundTask.addTaskDataCallback(_onTaskData);
    _load();
    _timer = Timer.periodic(const Duration(seconds: 20), (_) => _load());
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    FlutterForegroundTask.removeTaskDataCallback(_onTaskData);
    _timer?.cancel();
    super.dispose();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) _load();
  }

  void _onTaskData(Object data) {
    if (data is Map && data['type'] == 'job_done') _load();
  }

  Future<void> _load() async {
    try {
      final jobs = await _api.jobHistory();
      if (!mounted) return;
      setState(() {
        _jobs = jobs;
        _error = false;
      });
    } catch (_) {
      if (!mounted) return;
      // Keep whatever we had; only flag an error when nothing was loaded.
      if (_jobs == null) setState(() => _error = true);
    }
  }

  @override
  bool get wantKeepAlive => true;

  @override
  Widget build(BuildContext context) {
    super.build(context);
    final Widget body;
    if (_jobs == null) {
      body = _error ? _empty('Не удалось загрузить историю') : const Center(child: CircularProgressIndicator());
    } else if (_jobs!.isEmpty) {
      body = _empty('Пока нет заданий');
    } else {
      final groups = _groupByDay(_jobs!);
      body = ListView.separated(
        padding: const EdgeInsets.fromLTRB(16, 4, 16, 16),
        itemCount: groups.length,
        separatorBuilder: (_, _) => const SizedBox(height: 20),
        itemBuilder: (context, i) => GroupedSection(
          header: groups[i].title,
          children: [for (final j in groups[i].jobs) _JobRow(job: j)],
        ),
      );
    }
    return RefreshIndicator(onRefresh: _load, child: body);
  }

  Widget _empty(String text) {
    return ListView(
      children: [
        const SizedBox(height: 120),
        Center(child: Text(text, style: const TextStyle(color: AppColors.textSecondary))),
      ],
    );
  }

  List<_DayGroup> _groupByDay(List<JobHistoryEntry> jobs) {
    final now = DateTime.now();
    final today = DateTime(now.year, now.month, now.day);
    final groups = <String, _DayGroup>{};
    final order = <String>[];
    for (final j in jobs) {
      final local = (j.completedAt ?? j.createdAt).toLocal();
      final day = DateTime(local.year, local.month, local.day);
      final diff = today.difference(day).inDays;
      final title = diff == 0
          ? 'Сегодня'
          : diff == 1
              ? 'Вчера'
              : '${_two(day.day)}.${_two(day.month)}.${day.year}';
      if (!groups.containsKey(title)) {
        groups[title] = _DayGroup(title, []);
        order.add(title);
      }
      groups[title]!.jobs.add(j);
    }
    return [for (final t in order) groups[t]!];
  }
}

class _DayGroup {
  _DayGroup(this.title, this.jobs);
  final String title;
  final List<JobHistoryEntry> jobs;
}

String _two(int n) => n.toString().padLeft(2, '0');

class _JobRow extends StatelessWidget {
  const _JobRow({required this.job});
  final JobHistoryEntry job;

  static const _statusLabels = {
    'failed': 'Не удалось',
    'expired': 'Просрочено',
    'pending': 'В очереди',
    'assigned': 'Назначено',
    'running': 'Выполняется',
  };

  // Domain and IP checks are one kind of job for the executor, and the
  // VPN protocol is noise here -- just "VPN-ключ".
  static const _typeLabels = {
    'vpn_key': 'VPN-ключ',
    'ip': 'Домен/IP',
    'domain': 'Домен/IP',
  };

  @override
  Widget build(BuildContext context) {
    final title = _typeLabels[job.resourceType] ?? job.resourceType;
    final local = (job.completedAt ?? job.createdAt).toLocal();
    final time = '${_two(local.hour)}:${_two(local.minute)}';

    final String trailing;
    final Color color;
    if (job.status == 'completed') {
      trailing = '+${job.rewardAmount.toStringAsFixed(4)} USD';
      color = AppColors.success;
    } else if (job.status == 'failed') {
      trailing = _statusLabels['failed']!;
      color = AppColors.danger;
    } else {
      trailing = _statusLabels[job.status] ?? job.status;
      color = AppColors.textSecondary;
    }
    return GroupedTwoLineRow(title: title, subtitle: time, trailing: trailing, trailingColor: color);
  }
}
