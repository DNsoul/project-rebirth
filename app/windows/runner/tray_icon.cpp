#include "tray_icon.h"

#include <chrono>
#include <cstdio>
#include <cstring>

namespace {

constexpr UINT kIdStatus = 1;
constexpr UINT kIdToggle = 2;
constexpr UINT kIdShow = 3;
constexpr UINT kIdQuit = 4;

// A call to Explorer that takes longer than this is written to the log.
constexpr DWORD kSlowCallMs = 150;

const wchar_t* const kIconFiles[4] = {L"idle.ico", L"connected.ico",
                                      L"busy.ico", L"error.ico"};

HICON LoadTrayIcon(const std::wstring& dir, const wchar_t* file, int size) {
  std::wstring path = dir + L"\\" + file;
  HICON icon = static_cast<HICON>(LoadImageW(
      nullptr, path.c_str(), IMAGE_ICON, size, size, LR_LOADFROMFILE));
  if (icon == nullptr) {
    // Better the generic icon than none: the program must still be reachable.
    icon = LoadIconW(nullptr, IDI_APPLICATION);
  }
  return icon;
}

void ShowTheWindow(HWND window) {
  if (IsIconic(window)) {
    ShowWindow(window, SW_RESTORE);
  } else {
    ShowWindow(window, SW_SHOW);
  }
  SetForegroundWindow(window);
}

}  // namespace

TrayIcon::TrayIcon(HWND window, const std::wstring& icon_dir,
                   const std::wstring& log_path)
    : window_(window), log_path_(log_path) {
  UINT dpi = GetDpiForWindow(window);
  if (dpi == 0) {
    dpi = 96;
  }
  const int size = GetSystemMetricsForDpi(SM_CXSMICON, static_cast<int>(dpi));
  for (int i = 0; i < 4; ++i) {
    icons_[i] = LoadTrayIcon(icon_dir, kIconFiles[i], size);
  }
  // Explorer sends this to every window when it restarts, which empties the
  // notification area: the icon has to be put there again.
  taskbar_created_ = RegisterWindowMessageW(L"TaskbarCreated");
  worker_ = std::thread([this] { Worker(); });
}

TrayIcon::~TrayIcon() {
  {
    std::lock_guard<std::mutex> lock(mutex_);
    quit_ = true;
  }
  wake_.notify_all();
  // The worker may be inside a call that Explorer has not answered. It is not
  // waited for: the program is on its way out.
  bool finished;
  {
    std::unique_lock<std::mutex> lock(mutex_);
    finished = removed_signal_.wait_for(lock, std::chrono::seconds(1),
                                        [this] { return worker_done_; });
  }
  if (finished) {
    worker_.join();
  } else {
    worker_.detach();
  }
}

void TrayIcon::SetCommandHandler(
    std::function<void(const std::string&)> handler) {
  command_handler_ = std::move(handler);
}

void TrayIcon::Update(const TrayState& state) {
  {
    std::lock_guard<std::mutex> lock(mutex_);
    latest_ = state;
    has_latest_ = true;
  }
  wake_.notify_all();
}

void TrayIcon::Remove() {
  std::unique_lock<std::mutex> lock(mutex_);
  removed_ = false;
  remove_requested_ = true;
  wake_.notify_all();
  removed_signal_.wait_for(lock, std::chrono::milliseconds(1500),
                           [this] { return removed_; });
}

bool TrayIcon::HandleMessage(HWND window, UINT message, WPARAM wparam,
                             LPARAM lparam) {
  (void)wparam;
  if (message == kCallbackMessage) {
    switch (LOWORD(lparam)) {
      case WM_LBUTTONUP:
      case WM_LBUTTONDBLCLK:
        ShowTheWindow(window);
        break;
      case WM_RBUTTONUP:
      case WM_CONTEXTMENU:
        ShowMenu(window);
        break;
      default:
        break;
    }
    return true;
  }
  if (taskbar_created_ != 0 && message == taskbar_created_) {
    {
      std::lock_guard<std::mutex> lock(mutex_);
      add_again_ = true;
    }
    wake_.notify_all();
    return true;
  }
  return false;
}

void TrayIcon::ShowMenu(HWND window) {
  TrayState state;
  {
    std::lock_guard<std::mutex> lock(mutex_);
    state = latest_;
  }

  HMENU menu = CreatePopupMenu();
  if (menu == nullptr) {
    return;
  }
  AppendMenuW(menu, MF_STRING | MF_GRAYED, kIdStatus,
              state.status_line.c_str());
  AppendMenuW(menu, MF_SEPARATOR, 0, nullptr);
  AppendMenuW(menu, MF_STRING | (state.toggle_enabled ? 0 : MF_GRAYED),
              kIdToggle, state.toggle_label.c_str());
  AppendMenuW(menu, MF_STRING, kIdShow, state.show_label.c_str());
  AppendMenuW(menu, MF_SEPARATOR, 0, nullptr);
  AppendMenuW(menu, MF_STRING, kIdQuit, state.quit_label.c_str());

  POINT cursor;
  GetCursorPos(&cursor);
  // The window has to be in front for the menu to close when it is clicked away
  // from, and a message afterwards for it to close properly (both are in the
  // documentation of TrackPopupMenu).
  SetForegroundWindow(window);
  const UINT chosen = static_cast<UINT>(
      TrackPopupMenu(menu, TPM_RETURNCMD | TPM_RIGHTBUTTON | TPM_NONOTIFY,
                     cursor.x, cursor.y, 0, window, nullptr));
  PostMessageW(window, WM_NULL, 0, 0);
  DestroyMenu(menu);

  switch (chosen) {
    case kIdShow:
      ShowTheWindow(window);
      break;
    case kIdToggle:
      if (command_handler_) {
        command_handler_("toggle");
      }
      break;
    case kIdQuit:
      if (command_handler_) {
        command_handler_("quit");
      }
      break;
    default:
      break;
  }
}

void TrayIcon::Worker() {
  std::unique_lock<std::mutex> lock(mutex_);
  while (!quit_) {
    wake_.wait(lock, [this] {
      return quit_ || has_latest_ || add_again_ || remove_requested_;
    });
    if (quit_) {
      break;
    }
    if (remove_requested_) {
      remove_requested_ = false;
      lock.unlock();
      DeleteIcon();
      lock.lock();
      removed_ = true;
      removed_signal_.notify_all();
      continue;
    }
    const TrayState state = latest_;
    const bool again = add_again_;
    has_latest_ = false;
    add_again_ = false;
    lock.unlock();
    Apply(state, again);
    lock.lock();
  }
  worker_done_ = true;
  removed_signal_.notify_all();
}

void TrayIcon::Apply(const TrayState& state, bool add_again) {
  if (add_again) {
    added_ = false;
  }
  if (added_ && state.SameIconAndTip(applied_)) {
    return;  // Nothing Explorer needs to be told.
  }

  NOTIFYICONDATAW data;
  std::memset(&data, 0, sizeof(data));
  data.cbSize = sizeof(data);
  data.hWnd = window_;
  data.uID = 1;
  data.uFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP;
  data.uCallbackMessage = kCallbackMessage;
  const int index = (state.icon >= 0 && state.icon < 4) ? state.icon : 0;
  data.hIcon = icons_[index];
  wcsncpy_s(data.szTip, state.tooltip.c_str(), _TRUNCATE);

  const DWORD started = GetTickCount();
  BOOL ok;
  const wchar_t* what;
  if (!added_) {
    what = L"add";
    ok = Shell_NotifyIconW(NIM_ADD, &data);
    if (!ok) {
      // Already there (a second copy of the program, or a restart that left it).
      ok = Shell_NotifyIconW(NIM_MODIFY, &data);
    }
  } else {
    what = L"modify";
    ok = Shell_NotifyIconW(NIM_MODIFY, &data);
  }
  const DWORD took = GetTickCount() - started;
  if (ok) {
    added_ = true;
    applied_ = state;
  }
  if (took >= kSlowCallMs || !ok) {
    Log(what, took, ok != FALSE);
  }
}

void TrayIcon::DeleteIcon() {
  if (!added_) {
    return;
  }
  NOTIFYICONDATAW data;
  std::memset(&data, 0, sizeof(data));
  data.cbSize = sizeof(data);
  data.hWnd = window_;
  data.uID = 1;
  const DWORD started = GetTickCount();
  const BOOL ok = Shell_NotifyIconW(NIM_DELETE, &data);
  const DWORD took = GetTickCount() - started;
  added_ = false;
  if (took >= kSlowCallMs || !ok) {
    Log(L"delete", took, ok != FALSE);
  }
}

void TrayIcon::Log(const wchar_t* what, DWORD milliseconds, bool ok) {
  if (log_path_.empty()) {
    return;
  }
  FILE* file = nullptr;
  if (_wfopen_s(&file, log_path_.c_str(), L"ab") != 0 || file == nullptr) {
    return;
  }
  SYSTEMTIME now;
  GetLocalTime(&now);
  char line[200];
  char narrow[16];
  size_t i = 0;
  for (; what[i] != 0 && i < sizeof(narrow) - 1; ++i) {
    narrow[i] = static_cast<char>(what[i]);
  }
  narrow[i] = 0;
  const int n = _snprintf_s(
      line, sizeof(line), _TRUNCATE,
      "%04d-%02d-%02dT%02d:%02d:%02d.%03d tray: Shell_NotifyIcon %s took %lu ms%s\n",
      now.wYear, now.wMonth, now.wDay, now.wHour, now.wMinute, now.wSecond,
      now.wMilliseconds, narrow, static_cast<unsigned long>(milliseconds),
      ok ? "" : " and FAILED");
  if (n > 0) {
    fwrite(line, 1, static_cast<size_t>(n), file);
  }
  fclose(file);
}
