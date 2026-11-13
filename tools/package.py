"""Makes the prebuilt release: bin/release/godotmen-<version>-windows-x64.zip.

    python tools/package.py --src path\\to\\godot --version 0.1.0

Build everything first:
    python tools/build_godot.py --src path\\to\\godot      (editor and release template)
    ikemen\\build\\build_dll.cmd

The zip holds no game content. Its layout:
    godot/      the Godot editor and release template with the ikemen module
    ikemen/     ikemen.dll with SDL2 and libxmp, and Ikemen's game folder (data, font, external, stages)
    example/    the example project
    docs/       the guide
    licences/   Godot, Ikemen GO, this repository, and the Go modules and libraries inside ikemen.dll
    QUICKSTART.txt, Run-Example.cmd
"""
import argparse
import os
import shutil
import sys
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
IKEMEN = os.path.join(ROOT, "ikemen")

GODOT_FILES = {  # built name -> shipped name (a .console.exe starts the .exe of the same name)
    "godot.windows.editor.x86_64.exe": "godotmen_editor.exe",
    "godot.windows.editor.x86_64.console.exe": "godotmen_editor.console.exe",
    "godot.windows.template_release.x86_64.exe": "godotmen_template_release.exe",
    "godot.windows.template_release.x86_64.console.exe": "godotmen_template_release.console.exe",
}
IKEMEN_BIN = ["ikemen.dll", "SDL2.dll", "libxmp.dll"]
IKEMEN_DIRS = ["data", "font", "external", "stages"]

QUICKSTART = """Godotmen %(version)s - Ikemen GO inside Godot 4.7.1 (Windows x64)

This package has no characters or stages. Bring your own MUGEN / Ikemen pack.

1. Tell Ikemen which motif to use. Create the file ikemen\\save\\config.ini with:

       [Common]
       States = data/functions.zss, data/system.zss
       Fx =
       Lua =
       [Config]
       Motif = D:/packs/mypack/data/system.def
       FirstRun = 0

   (the system.def of your pack)

2. Run the example with two fighters of your pack:

       Run-Example.cmd D:/packs/mypack/chars/kfm/kfm.def D:/packs/mypack/chars/kfm/kfm.def

   Player 1: A/D walk, W jump, S crouch, J K L and U I O attack. Player 2 is the AI.

3. For your own game, open your project with godot\\godotmen_editor.exe, set its renderer to Compatibility,
   copy the ikemen folder into it (res://ikemen) and read docs\\GUIDE.md section 4. Export with
   godot\\godotmen_template_release.exe as the custom export template.

A stock Godot cannot run this: the ikemen module and three engine patches are built into the executables here.
Source and build instructions: the Godotmen repository.
"""

RUN_EXAMPLE = """@echo off
rem Runs the example with two fighters: Run-Example.cmd <p1 .def> <p2 .def>
if "%~2"=="" (
  echo Usage: Run-Example.cmd ^<path to p1 .def^> ^<path to p2 .def^>
  exit /b 1
)
"%~dp0godot\\godotmen_editor.exe" --path "%~dp0example" -- "--p1=%~1" "--p2=%~2"
"""

NOTICE = """ikemen.dll is Ikemen GO v1.0.0 (MIT, licences/IKEMEN_GO_LICENCE.txt) with the Godotmen host layer, built with Go.
It contains the Go runtime and standard library (BSD 3-clause, https://go.dev/LICENSE) and the Go modules whose
licences are in licences/go. SDL2.dll is SDL 2 (zlib licence, https://www.libsdl.org/license.php). libxmp.dll is
libxmp (MIT licence, https://github.com/libxmp/libxmp). The executables in godot/ are Godot Engine 4.7.1 (MIT,
licences/GODOT_LICENSE.txt; third-party notices in licences/GODOT_COPYRIGHT.txt) with the ikemen module.
"""


def copy(src, dst):
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    shutil.copyfile(src, dst)


def copy_tree(src, dst):
    for base, _dirs, files in os.walk(src):
        for name in files:
            p = os.path.join(base, name)
            copy(p, os.path.join(dst, os.path.relpath(p, src)))


def go_licences(dst):
    """The licence file of every Go module the DLL build downloaded (ikemen/bin/.cache/go-mod)."""
    cache = os.path.join(IKEMEN, "bin", ".cache", "go-mod")
    count = 0
    for base, dirs, files in os.walk(cache):
        if os.path.basename(base) == "cache" and os.path.dirname(base) == cache:
            dirs[:] = []
            continue
        if "@" not in os.path.basename(base):
            continue
        dirs[:] = []  # a module's root folder: <path>@<version>
        for name in files:
            if name.upper().startswith(("LICENSE", "LICENCE", "COPYING")):
                module = os.path.relpath(base, cache).replace(os.sep, "_")
                copy(os.path.join(base, name), os.path.join(dst, module + "_" + name))
                count += 1
    return count


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--src", default=os.environ.get("GODOT_SRC"), help="the Godot source folder that was built")
    ap.add_argument("--version", required=True)
    args = ap.parse_args()
    if not args.src:
        sys.exit("[package] pass --src <godot source> or set GODOT_SRC")

    name = "godotmen-%s-windows-x64" % args.version
    out = os.path.join(ROOT, "bin", "release")
    stage = os.path.join(out, name)
    if os.path.isdir(stage):
        shutil.rmtree(stage)

    missing = [n for n in GODOT_FILES if not os.path.isfile(os.path.join(args.src, "bin", n))]
    missing += [n for n in IKEMEN_BIN if not os.path.isfile(os.path.join(IKEMEN, "bin", n))]
    if missing:
        sys.exit("[package] not built yet: " + ", ".join(missing))

    for built, shipped in GODOT_FILES.items():
        copy(os.path.join(args.src, "bin", built), os.path.join(stage, "godot", shipped))
    for n in IKEMEN_BIN:
        copy(os.path.join(IKEMEN, "bin", n), os.path.join(stage, "ikemen", "bin", n))
    for d in IKEMEN_DIRS:
        copy_tree(os.path.join(IKEMEN, d), os.path.join(stage, "ikemen", d))
    for n in ("project.godot", "main.tscn", "main.gd"):
        copy(os.path.join(ROOT, "example", n), os.path.join(stage, "example", n))
    copy(os.path.join(ROOT, "docs", "GUIDE.md"), os.path.join(stage, "docs", "GUIDE.md"))
    copy(os.path.join(ROOT, "README.md"), os.path.join(stage, "README.md"))

    lic = os.path.join(stage, "licences")
    copy(os.path.join(ROOT, "LICENSE"), os.path.join(lic, "GODOTMEN_LICENSE.txt"))
    copy(os.path.join(IKEMEN, "LICENCE.txt"), os.path.join(lic, "IKEMEN_GO_LICENCE.txt"))
    copy(os.path.join(args.src, "LICENSE.txt"), os.path.join(lic, "GODOT_LICENSE.txt"))
    copy(os.path.join(args.src, "COPYRIGHT.txt"), os.path.join(lic, "GODOT_COPYRIGHT.txt"))
    modules = go_licences(os.path.join(lic, "go"))
    with open(os.path.join(lic, "NOTICE.txt"), "w", encoding="utf-8", newline="\r\n") as f:
        f.write(NOTICE)
    with open(os.path.join(stage, "QUICKSTART.txt"), "w", encoding="utf-8", newline="\r\n") as f:
        f.write(QUICKSTART % {"version": args.version})
    with open(os.path.join(stage, "Run-Example.cmd"), "w", encoding="utf-8", newline="\r\n") as f:
        f.write(RUN_EXAMPLE)

    zip_path = os.path.join(out, name + ".zip")
    if os.path.isfile(zip_path):
        os.remove(zip_path)
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for base, _dirs, files in os.walk(stage):
            for n in sorted(files):
                p = os.path.join(base, n)
                z.write(p, os.path.join(name, os.path.relpath(p, stage)))
    print("[package] %s (%.1f MB, %d Go module licences)" % (zip_path, os.path.getsize(zip_path) / 1048576, modules))
    return 0


if __name__ == "__main__":
    sys.exit(main())
