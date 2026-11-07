#ifndef RUNNER_FLUTTER_WINDOW_H_
#define RUNNER_FLUTTER_WINDOW_H_

#include <flutter/dart_project.h>
#include <flutter/encodable_value.h>
#include <flutter/flutter_view_controller.h>
#include <flutter/method_channel.h>

#include <memory>

#include "tray_icon.h"
#include "win32_window.h"

// Posted to the window of a running copy to ask it to leave, by the same way as
// the menu's "Exit" does: the tunnel comes down and the machine gets its names
// back. The setup and uninstall programs use it (Rebirth.exe --quit).
//
// It is a registered message, not a number picked from the WM_APP range: the
// icon in the notification area already uses WM_APP + 1 for the mouse events
// Explorer sends it, and a quit request with the same number made the program
// leave as soon as the pointer touched its icon. A registered message has the
// same number in every program that asks for the same name, and no other.
inline UINT QuitRequestMessage() {
  static const UINT message = RegisterWindowMessageW(L"Rebirth.QuitRequest");
  return message;
}

// A window that hosts a Flutter view, and the icon in the notification area.
class FlutterWindow : public Win32Window {
 public:
  // Creates a new FlutterWindow hosting a Flutter view running |project|.
  explicit FlutterWindow(const flutter::DartProject& project);
  virtual ~FlutterWindow();

  // Started with the system, the program goes straight to the notification area
  // and the window stays hidden until someone opens it.
  void SetStartHidden(bool hidden) { start_hidden_ = hidden; }

 protected:
  // Win32Window:
  bool OnCreate() override;
  void OnDestroy() override;
  LRESULT MessageHandler(HWND window, UINT const message, WPARAM const wparam,
                         LPARAM const lparam) noexcept override;

 private:
  // Makes the notification-area icon and the channel Dart drives it through.
  void SetUpTray();

  // The project to run.
  flutter::DartProject project_;

  bool start_hidden_ = false;

  // The Flutter instance hosted by this window.
  std::unique_ptr<flutter::FlutterViewController> flutter_controller_;

  std::unique_ptr<TrayIcon> tray_;
  std::unique_ptr<flutter::MethodChannel<flutter::EncodableValue>>
      tray_channel_;
};

#endif  // RUNNER_FLUTTER_WINDOW_H_
