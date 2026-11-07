#ifndef RUNNER_TRAY_ICON_H_
#define RUNNER_TRAY_ICON_H_

#include <windows.h>
#include <shellapi.h>

#include <condition_variable>
#include <functional>
#include <mutex>
#include <string>
#include <thread>

// What the icon in the notification area shows and offers. The strings are
// UTF-16, converted from what Dart sends; the labels come from Dart too, so
// that this file holds no text in any language.
struct TrayState {
  // 0 idle, 1 connected, 2 busy, 3 error. The same order as the icon files.
  int icon = 0;
  std::wstring tooltip;
  std::wstring status_line;
  std::wstring toggle_label;
  bool toggle_enabled = false;
  std::wstring show_label;
  std::wstring quit_label;

  bool SameIconAndTip(const TrayState& other) const {
    return icon == other.icon && tooltip == other.tooltip;
  }
};

// The icon in the notification area, and its menu.
//
// Talking to the notification area (Shell_NotifyIcon) is a conversation with
// Explorer, and when Explorer is busy, which it is for several seconds after a
// network adapter appears or goes away (exactly when a VPN connects or
// disconnects), the call waits for it. On the thread that runs the window that
// froze the whole program: no clicks, no repainting, a close button that did
// nothing for seconds. So every call to Explorer is made on a thread of its own.
// The window's thread only records what the icon should look like now and goes
// on; and what a click on the icon does (show the window, open the menu) it does
// itself, without waiting for anybody.
class TrayIcon {
 public:
  // The message Explorer sends the window when the icon is used.
  static constexpr UINT kCallbackMessage = WM_APP + 1;

  // |icon_dir| holds idle.ico, connected.ico, busy.ico and error.ico.
  // |log_path| gets a line whenever a call to Explorer was slow or failed.
  TrayIcon(HWND window, const std::wstring& icon_dir,
           const std::wstring& log_path);
  ~TrayIcon();

  TrayIcon(const TrayIcon&) = delete;
  TrayIcon& operator=(const TrayIcon&) = delete;

  // Records how the icon should look. Returns at once.
  void Update(const TrayState& state);

  // Takes the icon away. Waits a little for Explorer, but not for long.
  void Remove();

  // Called with every message the window gets. Returns true if it was ours.
  bool HandleMessage(HWND window, UINT message, WPARAM wparam, LPARAM lparam);

  // Called, on the window's thread, when a menu item that Dart has to act on is
  // chosen: "toggle" or "quit".
  void SetCommandHandler(std::function<void(const std::string&)> handler);

 private:
  void Worker();
  void Apply(const TrayState& state, bool add_again);
  void DeleteIcon();
  void ShowMenu(HWND window);
  void Log(const wchar_t* what, DWORD milliseconds, bool ok);

  HWND window_;
  HICON icons_[4] = {nullptr, nullptr, nullptr, nullptr};
  std::wstring log_path_;
  UINT taskbar_created_;
  std::function<void(const std::string&)> command_handler_;

  std::mutex mutex_;
  std::condition_variable wake_;
  std::condition_variable removed_signal_;
  TrayState latest_;
  bool has_latest_ = false;
  bool add_again_ = false;
  bool remove_requested_ = false;
  bool removed_ = false;
  bool quit_ = false;
  bool worker_done_ = false;

  // Worker thread only.
  bool added_ = false;
  TrayState applied_;

  std::thread worker_;
};

#endif  // RUNNER_TRAY_ICON_H_
