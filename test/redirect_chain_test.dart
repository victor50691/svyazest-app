import 'package:flutter_test/flutter_test.dart';
import 'package:svyazest_app/services/checkers/ip_checker.dart';

void main() {
  Uri u(String s) => Uri.parse(s);

  test('https redirect to another domain is trusted', () {
    final r = walkRedirects('twitter.com', [u('https://x.com/')]);
    expect(r.finalUri.host, 'x.com');
    expect(r.untrustedHop, isFalse);
  });

  test('relative and same-site hops', () {
    final r = walkRedirects('instagram.com', [u('https://www.instagram.com/'), u('/accounts/login/')]);
    expect(r.finalUri.toString(), 'https://www.instagram.com/accounts/login/');
    expect(r.untrustedHop, isFalse);
  });

  test('https -> http same site -> https foreign (injected) is untrusted', () {
    final r = walkRedirects('site.ru', [u('http://site.ru/'), u('https://warning.isp.ru/block')]);
    expect(r.finalUri.host, 'warning.isp.ru');
    expect(r.finalUri.scheme, 'https');
    expect(r.untrustedHop, isTrue);
  });

  test('https -> http same site -> back to https same site is trusted', () {
    final r = walkRedirects('site.ru', [u('http://www.site.ru/'), u('https://www.site.ru/')]);
    expect(r.untrustedHop, isFalse);
  });

  test('https -> http foreign: hop itself trusted (final http checked separately)', () {
    final r = walkRedirects('site.ru', [u('http://other.com/')]);
    expect(r.untrustedHop, isFalse);
    expect(r.finalUri.scheme, 'http');
  });

  test('no redirects', () {
    final r = walkRedirects('github.com', []);
    expect(r.finalUri.toString(), 'https://github.com/');
    expect(r.untrustedHop, isFalse);
  });
}
