import 'package:flutter/material.dart';

import '../services/attestation.dart';
import '../theme.dart';
import 'history_screen.dart';
import 'home_screen.dart';
import 'settings_screen.dart';

class MainShell extends StatefulWidget {
  const MainShell({super.key});

  @override
  State<MainShell> createState() => _MainShellState();
}

class _MainShellState extends State<MainShell> {
  int _index = 0;
  final _pages = PageController();

  @override
  void initState() {
    super.initState();
    // Fire-and-forget: the verdict lands on the server; the «Ещё» screen
    // reads it back from /profile.
    Attestation.run().catchError((_) => 'error');
  }

  @override
  void dispose() {
    _pages.dispose();
    super.dispose();
  }

  void _go(int i) {
    if (i == _index) return;
    _pages.animateToPage(i, duration: const Duration(milliseconds: 260), curve: Curves.easeOutCubic);
  }

  static const _tabs = [
    HomeTab(),
    HistoryTab(),
    SettingsTab(),
  ];

  static const _titles = ['Связь Есть?', 'История', 'Ещё'];

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: AnimatedSwitcher(
          duration: const Duration(milliseconds: 220),
          child: Text(_titles[_index], key: ValueKey(_index)),
        ),
      ),
      // Swipe left/right or tap the bar -- both slide the PageView. Each tab
      // keeps itself alive (AutomaticKeepAliveClientMixin) so its timers and
      // loaded data survive being scrolled out of view.
      body: PageView(
        controller: _pages,
        physics: const BouncingScrollPhysics(),
        onPageChanged: (i) => setState(() => _index = i),
        children: _tabs,
      ),
      bottomNavigationBar: DecoratedBox(
        decoration: const BoxDecoration(
          border: Border(top: BorderSide(color: AppColors.separator, width: 1)),
        ),
        child: NavigationBarTheme(
          data: NavigationBarThemeData(
            backgroundColor: AppColors.bg,
            surfaceTintColor: Colors.transparent,
            indicatorColor: Colors.transparent,
            overlayColor: const WidgetStatePropertyAll(Colors.transparent),
            height: 64,
            labelTextStyle: WidgetStateProperty.resolveWith(
              (states) => TextStyle(
                fontSize: 11,
                color: states.contains(WidgetState.selected) ? AppColors.textPrimary : AppColors.textSecondary,
              ),
            ),
            iconTheme: WidgetStateProperty.resolveWith(
              (states) => IconThemeData(
                size: 24,
                color: states.contains(WidgetState.selected) ? AppColors.textPrimary : AppColors.textSecondary,
              ),
            ),
          ),
          child: NavigationBar(
            selectedIndex: _index,
            onDestinationSelected: _go,
            destinations: const [
              NavigationDestination(icon: Icon(Icons.home_outlined), label: 'Главная'),
              NavigationDestination(icon: Icon(Icons.history_outlined), label: 'История'),
              NavigationDestination(icon: Icon(Icons.menu_outlined), label: 'Ещё'),
            ],
          ),
        ),
      ),
    );
  }
}
