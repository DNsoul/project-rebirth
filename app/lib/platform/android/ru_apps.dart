/// Russian apps are let past the tunnel on Android.
///
/// The fear is that such an app finds out the server's address and reports it
/// home, where it can end up on a block list. Its connections to Russian
/// servers already go past the tunnel, because the server's resolver gives out
/// their real addresses; what goes through the tunnel is whatever it sends
/// abroad (a "what is my address" service, foreign analytics, a CDN), and there
/// the far end sees the server's address. An app left out of the VPN entirely
/// sends nothing into the tunnel, asks the system's own resolver, and has no way
/// to learn where the tunnel leads.
///
/// The list ships with the app and changes with its updates. On top of it two
/// rules catch what the list does not name: package names of Russian owners
/// ([ruPrefixes]) and apps installed from a Russian store ([ruStores]).
library;

/// One app installed on the phone, as Android describes it.
class InstalledApp {
  const InstalledApp(this.package, {this.installer});

  /// The package name, e.g. `ru.sberbankmobile`.
  final String package;

  /// The package that installed it (a store), or null when the system does not
  /// say: preinstalled apps, and apps installed by hand on old Android.
  final String? installer;
}

/// Which apps go past the tunnel, and why, counted for the journal. Names are
/// not logged: the journal is handed over for diagnosis, and what someone has
/// installed is theirs.
class AppsPastTunnel {
  const AppsPastTunnel(this.packages, {this.byName = 0, this.byStore = 0});

  static const none = AppsPastTunnel([]);

  /// Sorted, so two answers can be compared.
  final List<String> packages;

  /// Named in the list, or with a Russian owner's package name.
  final int byName;

  /// Installed from a Russian store.
  final int byStore;

  String describe() => packages.isEmpty
      ? 'none'
      : '${packages.length} (by name $byName, from a Russian store $byStore)';
}

/// The app's own package. It must stay inside: it checks the tunnel by sending
/// through it.
const ownPackage = 'app.rebirth.vpn';

/// Stores whose apps go past the tunnel.
const ruStores = {
  'ru.vk.store', // RuStore
};

/// Package names that only Russian owners use: `ru.` is the reversed `.ru`
/// domain, the rest are the domains of Yandex, VK, Avito, Wildberries, Tinkoff,
/// Kaspersky and Dr.Web.
const ruPrefixes = [
  'ru.',
  'com.yandex.',
  'com.vkontakte.',
  'com.vk.',
  'com.avito.',
  'com.wildberries.',
  'com.tinkoff.',
  'com.kms.',
  'com.kaspersky.',
  'com.drweb.',
];

/// Russian apps whose package names say nothing of it. Most Russian apps start
/// with `ru.` or one of [ruPrefixes] and need no entry here.
const ruPackages = {
  'com.idamob.tinkoff.android', // Т-Банк
  'com.uma.musicvk', // VK Музыка
  'com.zvooq.openplay', // Звук
  'com.drweb',
  'com.octopod.russianpost.client', // Почта России
  'com.gnivts.selfemployed', // Мой налог
  'com.lamoda.lite',
  'com.icemobile.lenta.prod', // Лента
  'com.apegroup.mcdonaldrussia', // Вкусно — и точка
  'com.citymobil',
  'com.icq.mobile.client',
};

/// Foreign apps that are blocked or slowed in Russia. They need the tunnel
/// whichever store they came from: RuStore carries some of them.
const alwaysTunnelled = {
  'org.telegram.messenger',
  'org.telegram.messenger.web',
  'org.telegram.plus',
  'org.thunderdog.challegram', // Telegram X
  'com.whatsapp',
  'com.whatsapp.w4b',
  'com.viber.voip',
  'org.thoughtcrime.securesms', // Signal
  'com.discord',
  'com.google.android.youtube',
  'com.google.android.apps.youtube.music',
  'com.instagram.android',
  'com.facebook.katana',
  'com.facebook.orca',
  'com.twitter.android',
  'com.linkedin.android',
  'com.zhiliaoapp.musically', // TikTok
  'com.ss.android.ugc.trill', // TikTok
  'tv.twitch.android.app',
  'com.spotify.music',
  'com.netflix.mediaclient',
  'com.android.chrome',
  'org.mozilla.firefox',
  'com.openai.chatgpt',
  'com.anthropic.claude',
};

/// Picks the apps to let past the tunnel from those installed.
AppsPastTunnel appsPastTunnel(Iterable<InstalledApp> installed) {
  final byName = <String>{};
  final byStore = <String>{};
  for (final app in installed) {
    final p = app.package;
    if (p.isEmpty || p == ownPackage || alwaysTunnelled.contains(p)) continue;
    if (ruPackages.contains(p) || ruPrefixes.any(p.startsWith)) {
      byName.add(p);
    } else if (ruStores.contains(app.installer)) {
      byStore.add(p);
    }
  }
  return AppsPastTunnel(
    [...byName, ...byStore]..sort(),
    byName: byName.length,
    byStore: byStore.length,
  );
}

/// Whether two sorted lists name the same apps.
bool sameApps(List<String> a, List<String> b) {
  if (a.length != b.length) return false;
  for (var i = 0; i < a.length; i++) {
    if (a[i] != b[i]) return false;
  }
  return true;
}
