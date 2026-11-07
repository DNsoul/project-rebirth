#include <flutter/dart_project.h>
#include <flutter/flutter_view_controller.h>
#include <windows.h>

#include <algorithm>
#include <string>
#include <vector>

#include "flutter_window.h"
#include "utils.h"

namespace {

// The window title, and the class every window of the Flutter runner has.
constexpr wchar_t kTitle[] = L"Rebirth";
constexpr wchar_t kWindowClass[] = L"FLUTTER_RUNNER_WIN32_WINDOW";

bool Has(const std::vector<std::string>& arguments, const char* flag) {
  return std::find(arguments.begin(), arguments.end(), std::string(flag)) !=
         arguments.end();
}

// Rebirth.exe --quit: asks the copy that is running to leave the way its menu's
// "Exit" does, and waits until it has. Nothing else is started: no engine, no
// window, no icon. Succeeds at once when no copy is running.
//
// The setup and the uninstaller run this before touching any files. Without it
// the only way to get the program out of the way is to kill it, and a program
// that is killed leaves its tunnel and its name-resolution rule behind.
int AskRunningCopyToQuit() {
  const HWND window = FindWindowExW(nullptr, nullptr, kWindowClass, kTitle);
  if (window == nullptr) {
    return EXIT_SUCCESS;
  }
  PostMessageW(window, QuitRequestMessage(), 0, 0);
  // Taking the tunnel down and giving the names back takes seconds; thirty is
  // generous. The program ends with its window.
  for (int waited = 0; waited < 300; ++waited) {
    Sleep(100);
    if (!IsWindow(window)) {
      return EXIT_SUCCESS;
    }
  }
  return EXIT_FAILURE;
}

}  // namespace

int APIENTRY wWinMain(_In_ HINSTANCE instance, _In_opt_ HINSTANCE prev,
                      _In_ wchar_t *command_line, _In_ int show_command) {
  // Attach to console when present (e.g., 'flutter run') or create a
  // new console when running with a debugger.
  if (!::AttachConsole(ATTACH_PARENT_PROCESS) && ::IsDebuggerPresent()) {
    CreateAndAttachConsole();
  }

  std::vector<std::string> command_line_arguments =
      GetCommandLineArguments();

  if (Has(command_line_arguments, "--quit")) {
    return AskRunningCopyToQuit();
  }
  const bool start_hidden = Has(command_line_arguments, "--autostart");

  // Initialize COM, so that it is available for use in the library and/or
  // plugins.
  ::CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);

  flutter::DartProject project(L"data");

  project.set_dart_entrypoint_arguments(std::move(command_line_arguments));

  FlutterWindow window(project);
  window.SetStartHidden(start_hidden);
  // A narrow window, like a phone: one button in the middle of it, and room above
  // and below for the offer of an update and for an error.
  Win32Window::Point origin(200, 120);
  Win32Window::Size size(340, 560);
  if (!window.Create(kTitle, origin, size)) {
    return EXIT_FAILURE;
  }
  window.SetQuitOnClose(true);

  ::MSG msg;
  while (::GetMessage(&msg, nullptr, 0, 0)) {
    ::TranslateMessage(&msg);
    ::DispatchMessage(&msg);
  }

  ::CoUninitialize();
  return EXIT_SUCCESS;
}
