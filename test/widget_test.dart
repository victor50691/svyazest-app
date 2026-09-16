// Smoke test: the app should at least boot to either the pairing screen or
// the main shell without throwing, regardless of secure-storage state.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:svyazest_app/main.dart';

void main() {
  testWidgets('App boots to a screen without throwing', (WidgetTester tester) async {
    await tester.pumpWidget(const SvyazEstApp());
    await tester.pump();
    expect(find.byType(MaterialApp), findsOneWidget);
  });
}
