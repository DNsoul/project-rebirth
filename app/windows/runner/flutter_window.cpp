#include "flutter_window.h"

#include <flutter/standard_method_codec.h>

#include <optional>
#include <string>

#include "flutter/generated_plugin_registrant.h"

namespace {

std::wstring ToWide(const std::string& utf8) {
  if (utf8.empty()) {
    return std::wstring();
  }
  const int length = MultiByteToWideChar(CP_UTF8, 0, utf8.data(),
                                         static_cast<int>(utf8.size()),
                                         nullptr, 0);
  if (length <= 0) {
    return std::wstring();
  }
  std::wstring wide(static_cast<size_t>(length), L'\0');
  MultiByteToWideChar(CP_UTF8, 0, utf8.data(), static_cast<int>(utf8.size()),
                      &wide[0], length);
  return wide;
}

std::wstring TextOf(const flutter::EncodableMap& map, const char* key) {
  auto found = map.find(flutter::EncodableValue(std::string(key)));
  if (found == map.end()) {
    return std::wstring();
  }
  const std::string* text = std::get_if<std::string>(&found->second);
  return text ? ToWide(*text) : std::wstring();
}

int IntOf(const flutter::EncodableMap& map, const char* key) {
  auto found = map.find(flutter::EncodableValue(std::string(key)));
  if (found == map.end()) {
    return 0;
  }
  const int32_t* number = std::get_if<int32_t>(&found->second);
  return number ? static_cast<int>(*number) : 0;
}

bool BoolOf(const flutter::EncodableMap& map, const char* key) {
  auto found = map.find(flutter::EncodableValue(std::string(key)));
  if (found == map.end()) {
    return false;
  }
  const bool* flag = std::get_if<bool>(&found->second);
  return flag ? *flag : false;
}

// The directory the program is in, without a trailing separator.
std::wstring ProgramDirectory() {
  wchar_t path[MAX_PATH];
  const DWORD length = GetModuleFileNameW(nullptr, path, MAX_PATH);
  std::wstring full(path, length);
  const size_t slash = full.find_last_of(L'\\');
  return slash == std::wstring::npos ? std::wstring() : full.substr(0, slash);
}

std::wstring LocalAppData() {
  wchar_t buffer[MAX_PATH];
  const DWORD length = GetEnvironmentVariableW(L"LOCALAPPDATA", buffer, MAX_PATH);
  if (length == 0 || length >= MAX_PATH) {
    return std::wstring();
  }
  return std::wstring(buffer, length);
}

}  // namespace

FlutterWindow::FlutterWindow(const flutter::DartProject& project)
    : project_(project) {}

FlutterWindow::~FlutterWindow() {}

bool FlutterWindow::OnCreate() {
  if (!Win32Window::OnCreate()) {
    return false;
  }

  RECT frame = GetClientArea();

  // The size here must match the window dimensions to avoid unnecessary surface
  // creation / destruction in the startup path.
  flutter_controller_ = std::make_unique<flutter::FlutterViewController>(
      frame.right - frame.left, frame.bottom - frame.top, project_);
  // Ensure that basic setup of the controller was successful.
  if (!flutter_controller_->engine() || !flutter_controller_->view()) {
    return false;
  }
  RegisterPlugins(flutter_controller_->engine());
  SetChildContent(flutter_controller_->view()->GetNativeWindow());
  SetUpTray();

  flutter_controller_->engine()->SetNextFrameCallback([&]() {
    if (!start_hidden_) {
      this->Show();
    }
  });

  // Flutter can complete the first frame before the "show window" callback is
  // registered. The following call ensures a frame is pending to ensure the
  // window is shown. It is a no-op if the first frame hasn't completed yet.
  flutter_controller_->ForceRedraw();

  return true;
}

void FlutterWindow::SetUpTray() {
  const std::wstring icons = ProgramDirectory() + L"\\data\\flutter_assets\\assets\\tray";
  const std::wstring appdata = LocalAppData();
  const std::wstring log =
      appdata.empty() ? std::wstring() : appdata + L"\\awg-rotator\\native.log";
  tray_ = std::make_unique<TrayIcon>(GetHandle(), icons, log);

  tray_channel_ =
      std::make_unique<flutter::MethodChannel<flutter::EncodableValue>>(
          flutter_controller_->engine()->messenger(), "awg/tray",
          &flutter::StandardMethodCodec::GetInstance());

  // A menu item that Dart has to act on.
  tray_->SetCommandHandler([this](const std::string& command) {
    tray_channel_->InvokeMethod(
        "command", std::make_unique<flutter::EncodableValue>(command));
  });

  tray_channel_->SetMethodCallHandler(
      [this](const flutter::MethodCall<flutter::EncodableValue>& call,
             std::unique_ptr<flutter::MethodResult<flutter::EncodableValue>>
                 result) {
        if (call.method_name() == "update") {
          const auto* map = std::get_if<flutter::EncodableMap>(call.arguments());
          if (map == nullptr) {
            result->Error("bad-arguments", "update needs a map");
            return;
          }
          TrayState state;
          state.icon = IntOf(*map, "icon");
          state.tooltip = TextOf(*map, "tooltip");
          state.status_line = TextOf(*map, "status");
          state.toggle_label = TextOf(*map, "toggleLabel");
          state.toggle_enabled = BoolOf(*map, "toggleEnabled");
          state.show_label = TextOf(*map, "showLabel");
          state.quit_label = TextOf(*map, "quitLabel");
          // Returns at once: the call to Explorer is made on another thread.
          tray_->Update(state);
          result->Success();
        } else if (call.method_name() == "remove") {
          tray_->Remove();
          result->Success();
        } else {
          result->NotImplemented();
        }
      });
}

void FlutterWindow::OnDestroy() {
  tray_channel_ = nullptr;
  tray_ = nullptr;
  if (flutter_controller_) {
    flutter_controller_ = nullptr;
  }

  Win32Window::OnDestroy();
}

LRESULT
FlutterWindow::MessageHandler(HWND hwnd, UINT const message,
                              WPARAM const wparam,
                              LPARAM const lparam) noexcept {
  // Closing the window hides it; the program lives in the notification area and
  // is ended from its menu. This is done here, before anything else sees the
  // message, and not by telling Dart to do it: the answer to the close button
  // must not depend on how busy anything else is.
  if (message == WM_CLOSE) {
    ShowWindow(hwnd, SW_HIDE);
    return 0;
  }

  // Another program (the setup, the uninstaller) asks this one to leave. It is
  // handed to Dart as the menu's "Exit" would be, since that is the path that
  // puts the machine back as it found it.
  if (message == QuitRequestMessage()) {
    if (tray_channel_) {
      tray_channel_->InvokeMethod(
          "command",
          std::make_unique<flutter::EncodableValue>(std::string("quit")));
    }
    return 0;
  }

  // The icon in the notification area.
  if (tray_ && tray_->HandleMessage(hwnd, message, wparam, lparam)) {
    return 0;
  }

  // Give Flutter, including plugins, an opportunity to handle window messages.
  if (flutter_controller_) {
    std::optional<LRESULT> result =
        flutter_controller_->HandleTopLevelWindowProc(hwnd, message, wparam,
                                                      lparam);
    if (result) {
      return *result;
    }
  }

  switch (message) {
    case WM_FONTCHANGE:
      flutter_controller_->engine()->ReloadSystemFonts();
      break;
  }

  return Win32Window::MessageHandler(hwnd, message, wparam, lparam);
}
