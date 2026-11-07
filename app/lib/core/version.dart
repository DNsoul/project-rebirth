/// Which build this is. Kept next to the code, not read from the system, so that
/// it is the same on both platforms and a test can hold it to `pubspec.yaml`:
/// `version: 1.0.0+1` there is `appVersion` and `appBuild` here.
///
/// The build number is what an announcement of a new version is compared with.
/// It grows with every release.
library;

const appVersion = '1.0.1';
const appBuild = 2;
