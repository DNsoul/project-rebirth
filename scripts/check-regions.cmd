@echo off
rem Double-click this file to check which server regions open from this internet.
rem It runs check-regions.ps1 next to it; the window stays open until a key is pressed.
title Rebirth: region check
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0check-regions.ps1" -NoPause %*
echo.
pause
