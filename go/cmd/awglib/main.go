// Command awglib is built as a shared library, libawg.so, that the Android app
// loads. The functions it exports are in exports_android.go; on every other
// system there is nothing to build, and this empty program only keeps the
// package valid for the tools that look at the whole module.
//
// Build it with scripts/build-android-lib.ps1.
package main

func main() {}
