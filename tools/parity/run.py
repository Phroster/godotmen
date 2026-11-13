"""Parity test: the same fight in stock Ikemen GO v1.0.0 and in ikemen.dll, compared tick by tick.

    python tools/parity/run.py --pack D:\\packs\\mypack --p1 kfm --p2 kfm
    python tools/parity/run.py --pack D:\\packs\\mypack --p1 kfm --p2 suave --stage stages/stage0.def --ticks 3000 --seed 7

Both sides run an AI level 8 against an AI level 8 from the same random seed. src/host_dump.go, which compiles
unchanged into stock Ikemen GO, writes one line per logic tick: the random seed, the round state and, for every
character and helper, its state, animation, life, facing, position and velocity. The two files must be identical.
Equal seeds on every tick mean every random draw of the engine and of the characters' code happened in the same order.

It compares simulation state, not pixels or mixed sound.

What it sets up, all under bin/parity (nothing in the pack is changed):
  stock/   Ikemen GO v1.0.0 from GitHub with host_dump.go and its one-line hook, built with MSYS2 clang64. The stock
           executable opens a window for the length of the fight.
  pack/    a view of the pack through directory junctions and hard links. The motif's fight.def is a copy without
           the lifebar fonts, because stock Ikemen GO cannot load OpenType fonts and stops on them; the lifebar's
           text does not take part in the simulation.
  root/    an Ikemen game folder for the DLL run (junctions to ikemen/data, font, external, with its own save/).

Needs ikemen/bin/ikemen.dll and ikemen/bin/ovhost.exe (ikemen/build/build_dll.cmd, tools/ovhost/build.sh), MSYS2 at
C:\\msys64 with the clang64 packages go, clang, pkgconf, SDL2, libxmp, ffmpeg (stock Ikemen GO links FFmpeg), and git.
"""
import argparse
import os
import re
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
IKEMEN = os.path.join(ROOT, "ikemen")
WORK = os.path.join(ROOT, "bin", "parity")
MSYS_BASH = r"C:\msys64\usr\bin\bash.exe"
CLANG_BIN = r"C:\msys64\clang64\bin"
STOCK_TAG = "v1.0.0"
STOCK_URL = "https://github.com/ikemen-engine/Ikemen-GO.git"
HOOK = "func (s *System) action() {"

CONFIG = """[Common]
States = data/functions.zss, data/system.zss
Fx =
Lua =
[Config]
Motif = %s
FirstRun = 0
[Video]
Fullscreen = 0
VSync = 0
"""


def log(msg):
    print("[parity] " + msg, flush=True)


def fwd(path):
    return os.path.abspath(path).replace("\\", "/")


def junction(link, target):
    if os.path.lexists(link):
        return
    os.makedirs(os.path.dirname(link), exist_ok=True)
    subprocess.run(["cmd", "/c", "mklink", "/J", link, target], check=True, capture_output=True)


def build_stock():
    stock = os.path.join(WORK, "stock")
    exe = os.path.join(stock, "Ikemen_GO.exe")
    dump_src = os.path.join(IKEMEN, "src", "host_dump.go")
    dump_dst = os.path.join(stock, "src", "host_dump.go")
    if not os.path.isdir(os.path.join(stock, "src")):
        log("cloning stock Ikemen GO %s" % STOCK_TAG)
        subprocess.run(["git", "clone", "-q", "--depth", "1", "--branch", STOCK_TAG, STOCK_URL, stock], check=True)
    with open(dump_src, "rb") as f:
        want = f.read()
    have = b""
    if os.path.isfile(dump_dst):
        with open(dump_dst, "rb") as f:
            have = f.read()
    if os.path.isfile(exe) and have == want:
        return exe
    shutil.copyfile(dump_src, dump_dst)
    system_go = os.path.join(stock, "src", "system.go")
    with open(system_go, encoding="utf-8", newline="") as f:
        text = f.read()
    if "hostDumpTick()" not in text:
        if HOOK not in text:
            sys.exit("[parity] stock system.go has no '%s'" % HOOK)
        text = text.replace(HOOK, HOOK + "\n\thostDumpTick()", 1)
        with open(system_go, "w", encoding="utf-8", newline="") as f:
            f.write(text)
    log("building stock Ikemen GO (a few minutes the first time)")
    cache = fwd(os.path.join(WORK, "cache"))
    script = (
        "set -e; export PATH=/clang64/bin:/usr/bin:$PATH GOROOT=/clang64/lib/go CC=clang CXX=clang++ CGO_ENABLED=1 "
        "GOEXPERIMENT=arenas GOTOOLCHAIN=local; C=\"$(cygpath -u '%s')\"; mkdir -p \"$C/tmp\"; "
        "export GOPATH=\"$C/go-path\" GOCACHE=\"$C/go-cache\" GOTMPDIR=\"$C/tmp\" TEMP=\"$C/tmp\" TMP=\"$C/tmp\"; "
        "cd \"$(cygpath -u '%s')\"; go build -trimpath -o Ikemen_GO.exe ./src" % (cache, fwd(stock))
    )
    r = subprocess.run([MSYS_BASH, "--noprofile", "--norc", "-c", script])
    if r.returncode != 0 or not os.path.isfile(exe):
        sys.exit("[parity] the stock build failed")
    return exe


def find_motif_dir(pack):
    """<pack>/data, or the first folder under it, that holds fight.def (the same rule as the DLL's)."""
    data = os.path.join(pack, "data")
    candidates = [data] + sorted(os.path.join(data, n) for n in os.listdir(data) if os.path.isdir(os.path.join(data, n)))
    for d in candidates:
        if any(n.lower() == "fight.def" for n in os.listdir(d)):
            return d
    sys.exit("[parity] no fight.def under " + data)


def mirror_pack(pack):
    """A view of the pack whose fight.def has no lifebar fonts. Junctions and hard links: nothing is copied but two
    small text files, and the pack itself is never written to."""
    motif = find_motif_dir(pack)
    view = os.path.join(WORK, "pack")
    if os.path.isdir(view):  # junctions are removed as links (rmdir never follows them), then the real folders
        for base, dirs, _files in os.walk(view, topdown=True):
            for d in list(dirs):
                p = os.path.join(base, d)
                if os.path.isjunction(p):
                    os.rmdir(p)
                    dirs.remove(d)
        shutil.rmtree(view)
    os.makedirs(view)
    rel = os.path.relpath(motif, pack)
    for name in os.listdir(pack):
        src = os.path.join(pack, name)
        if os.path.isdir(src) and name.lower() != "data":
            junction(os.path.join(view, name), src)
    view_motif = os.path.join(view, rel)
    os.makedirs(view_motif)
    for name in os.listdir(motif):
        src, dst = os.path.join(motif, name), os.path.join(view_motif, name)
        if os.path.isdir(src):
            junction(dst, src)
        elif name.lower() == "fight.def":
            with open(src, encoding="latin-1", newline="") as f:
                text = f.read()
            text = re.sub(r"(?im)^(\s*font[2-9]\d*\s*=)", r";\1", text)
            with open(dst, "w", encoding="latin-1", newline="") as f:
                f.write(text)
        else:
            try:
                os.link(src, dst)
            except OSError:
                shutil.copyfile(src, dst)
    system_def = next(n for n in os.listdir(view_motif) if n.lower() == "system.def")
    return view, os.path.join(view_motif, system_def)


def find_def(view, name):
    folder = os.path.join(view, "chars", name)
    if name.lower().endswith(".def"):
        return fwd(os.path.join(view, "chars", name))
    for n in sorted(os.listdir(folder)):
        if n.lower() == name.lower() + ".def":
            return fwd(os.path.join(folder, n))
    sys.exit("[parity] no %s.def in %s" % (name, folder))


def find_stage(view, stage):
    if stage:
        return fwd(os.path.join(view, stage))
    stages = os.path.join(view, "stages")
    for base, _dirs, files in os.walk(stages):
        for n in sorted(files):
            if n.lower().endswith(".def"):
                return fwd(os.path.join(base, n))
    sys.exit("[parity] no stage .def under " + stages)


def run(cmd, cwd, env, log_path, timeout):
    with open(log_path, "w", encoding="utf-8", errors="replace") as out:
        try:
            return subprocess.run(cmd, cwd=cwd, env=env, stdout=out, stderr=subprocess.STDOUT, timeout=timeout).returncode
        except subprocess.TimeoutExpired:
            return -1


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--pack", required=True, help="the pack folder (it holds chars/, data/, stages/)")
    ap.add_argument("--p1", required=True, help="fighter folder under chars/, or folder/file.def")
    ap.add_argument("--p2", required=True)
    ap.add_argument("--stage", help="stage .def relative to the pack (default: the first one under stages/)")
    ap.add_argument("--ticks", type=int, default=1500)
    ap.add_argument("--seed", type=int, default=7)
    args = ap.parse_args()

    ovhost = os.path.join(IKEMEN, "bin", "ovhost.exe")
    if not os.path.isfile(ovhost) or not os.path.isfile(os.path.join(IKEMEN, "bin", "ikemen.dll")):
        sys.exit("[parity] build ikemen/bin/ikemen.dll and ovhost.exe first")
    os.makedirs(WORK, exist_ok=True)
    stock_exe = build_stock()
    view, motif = mirror_pack(os.path.abspath(args.pack))
    p1, p2, stage = find_def(view, args.p1), find_def(view, args.p2), find_stage(view, args.stage)
    config = CONFIG % fwd(motif)

    stock_root = os.path.dirname(stock_exe)
    root = os.path.join(WORK, "root")
    for name in ("data", "font", "external", "stages"):
        junction(os.path.join(root, name), os.path.join(IKEMEN, name))
    for r in (stock_root, root):
        os.makedirs(os.path.join(r, "save"), exist_ok=True)
        with open(os.path.join(r, "save", "config.ini"), "w", encoding="utf-8") as f:
            f.write(config)

    ref, got = os.path.join(WORK, "stock.txt"), os.path.join(WORK, "dll.txt")
    for p in (ref, got):
        if os.path.isfile(p):
            os.remove(p)
    env = dict(os.environ, IKEMEN_DUMP_TICKS=str(args.ticks), IKEMEN_DUMP_SEED=str(args.seed))
    env["PATH"] = CLANG_BIN + os.pathsep + env.get("PATH", "")

    log("stock Ikemen GO: %s vs %s on %s" % (os.path.basename(p1), os.path.basename(p2), os.path.basename(stage)))
    run([stock_exe, "-p1", p1, "-p2", p2, "-p1.ai", "8", "-p2.ai", "8", "-s", stage, "-nosound", "-nojoy", "-windowed"],
        stock_root, dict(env, IKEMEN_DUMP=fwd(ref), IKEMEN_DUMP_EXIT="1"), os.path.join(WORK, "stock.log"), 600)
    log("ikemen.dll, parity mode")
    run([ovhost, "--parity", "--root", fwd(root), "--p1", p1, "--p2", p2, "--ai1", "8", "--ai2", "8", "--stage", stage,
         "--ticks", str(args.ticks + 900), "--every", "100000", "--limit", "100", "--out", fwd(os.path.join(WORK, "frames"))],
        os.path.join(IKEMEN, "bin"), dict(env, IKEMEN_DUMP=fwd(got)), os.path.join(WORK, "dll.log"), 600)

    for p, what in ((ref, "stock"), (got, "dll")):
        if not os.path.isfile(p):
            # (stock Ikemen GO refuses some packs outright, e.g. state controllers of MUGEN hooks it does not know)
            with open(os.path.join(WORK, what + ".log"), encoding="utf-8", errors="replace") as f:
                lines = f.read().splitlines()
            at = next((i for i, line in enumerate(lines) if line.startswith("Panic")), max(0, len(lines) - 3))
            print("\n".join(line[:300] for line in lines[at:at + 3]))
            sys.exit("[parity] NO RESULT: the %s run could not play this fight; see bin/parity/%s.log" % (what, what))
    with open(ref) as f:
        a = f.read().splitlines()
    with open(got) as f:
        b = f.read().splitlines()
    if len(a) < args.ticks or len(b) < args.ticks:
        sys.exit("[parity] FAILED: dumps are short (stock %d, dll %d of %d ticks); see bin/parity/*.log"
                 % (len(a), len(b), args.ticks))
    for i in range(args.ticks):
        if a[i] != b[i]:
            print("[parity] FAILED: first difference at tick %d of %d" % (i, args.ticks))
            print("stock: " + a[i][:600])
            print("dll:   " + b[i][:600])
            return 1
    log("PASSED: %d ticks identical (state of every character and helper, and the random seed)" % args.ticks)
    return 0


if __name__ == "__main__":
    sys.exit(main())
