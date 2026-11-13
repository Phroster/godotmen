@echo off
rem Builds ikemen\bin\ikemen.dll with MSYS2 clang64 (expects MSYS2 at C:\msys64).
if not exist C:\msys64\usr\bin\bash.exe (
  echo MSYS2 not found at C:\msys64. Install it, then: pacman -S mingw-w64-clang-x86_64-{go,clang,pkgconf,SDL2,libxmp}
  exit /b 1
)
C:\msys64\usr\bin\bash.exe --noprofile --norc "%~dp0build_dll.sh"
